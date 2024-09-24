package webhook

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"gopkg.in/go-playground/webhooks.v5/docker"

	"github.com/argoproj-labs/argocd-image-updater/pkg/log"
	"github.com/argoproj-labs/argocd-image-updater/pkg/options"
	"github.com/argoproj-labs/argocd-image-updater/pkg/registry"
	"github.com/argoproj-labs/argocd-image-updater/pkg/tag"
)

type WebhookEvent struct {
	RegistryPrefix string
	RepoName       string
	ImageName      string
	TagName        string
	CreatedAt      time.Time
	Digest         string
}

type Event string

type RegistryWebhook interface {
	New(secret string) (RegistryWebhook, error)
	Parse(r *http.Request, events ...Event) (*WebhookEvent, error)
}

var webhookEventCh (chan WebhookEvent) = make(chan WebhookEvent)

// GetWebhookEventChan return a chan for WebhookEvent
func GetWebhookEventChan() chan WebhookEvent {
	return webhookEventCh
}

// StartRegistryHookServer starts a new HTTP server for registry hook on given port
func StartRegistryHookServer(port int) chan error {
	errCh := make(chan error)
	go func() {
		sm := http.NewServeMux()

		regPrefixes := registry.ConfiguredEndpoints()
		log.Debugf("Configured registry endpoints: %v", regPrefixes)
		for _, prefix := range regPrefixes {
			var regPrefix string = prefix
			if regPrefix == "" {
				regPrefix = "docker.io"
			}
			var path string = fmt.Sprintf("/api/webhook/%s", regPrefix)
			log.Infof("Registering webhook handler for path: %s", path)
			sm.HandleFunc(path, webhookHandler)
		}
		log.Infof("Starting webhook server on port %d", port)
		errCh <- http.ListenAndServe(fmt.Sprintf(":%d", port), sm)
	}()
	return errCh
}

func getTagMetadata(regPrefix string, imageName string, tagStr string) (*tag.TagInfo, error) {
	log.Infof("Getting tag metadata for %s/%s:%s", regPrefix, imageName, tagStr)
	// DEBU[0003] Getting tag metadata for 665386098866.dkr.ecr.us-east-1.amazonaws.com/665386098866.dkr.ecr.us-east-1.amazonaws.com/feishu-alert-proxy:prod-0.0.4-557a1cdc

	rep, err := registry.GetRegistryEndpoint(regPrefix)
	if err != nil {
		log.Errorf("Could not get registry endpoint for %s: %v", regPrefix, err)
		return nil, err
	}
	log.Debugf("Got registry endpoint for %s: %+v", regPrefix, rep)

	regClient, err := registry.NewClient(rep, rep.Username, rep.Password)
	if err != nil {
		log.Errorf("Could not create new registry client for %s: %v", regPrefix, err)
		return nil, err
	}
	log.Debugf("Created new registry client for %s", regPrefix)

	var nameInRegistry string
	// if len := len(strings.Split(imageName, "/")); len == 1 && rep.DefaultNS != "" {
	// 	nameInRegistry = rep.DefaultNS + "/" + imageName
	// 	log.Debugf("Using canonical image name '%s' for image '%s'", nameInRegistry, imageName)
	// } else {
	// 	nameInRegistry = imageName
	// }
	// 兼容ECR, ECR nameInRegistry 只需要镜像名称,不需要仓库地址
	if strings.Contains(regPrefix, "ecr") {
		parts := strings.Split(imageName, "/")
		nameInRegistry = parts[len(parts)-1] // 这将得到 "feishu-alert-proxy"
	} else {
		// 原有的逻辑
		if len := len(strings.Split(imageName, "/")); len == 1 && rep.DefaultNS != "" {
			nameInRegistry = rep.DefaultNS + "/" + imageName
		} else {
			nameInRegistry = imageName
		}
	}

	err = regClient.NewRepository(nameInRegistry)
	if err != nil {
		log.Errorf("Could not create new repository for %s: %v", nameInRegistry, err)
		return nil, err
	}
	log.Debugf("Created new repository for %s", nameInRegistry)

	// manifest, err := regClient.ManifestForTag(tagStr)
	// if err != nil {
	// 	log.Errorf("Could not fetch manifest for %s:%s - no manifest returned by registry: %v", regPrefix, tagStr, err)
	// 	return nil, err
	// }

	// 获取所有tags 验证权限是否有问题
	// log.Debugf("Attempting to fetch manifest for %s:%s", nameInRegistry, tagStr)
	// tags, err := regClient.Tags()
	// if err != nil {
	// 	log.Errorf("tags error: %v", err)
	// 	return nil, err
	// }
	// log.Debugf("tags: %v", tags)

	manifest, err := regClient.ManifestForTag(tagStr)
	if err != nil {
		log.Errorf("Could not fetch manifest for %s:%s: %v", nameInRegistry, tagStr, err)
		return nil, err
	}
	log.Debugf("Successfully fetched manifest for %s:%s", nameInRegistry, tagStr)

	log.Debugf("Fetched manifest for %s:%s", regPrefix, tagStr)

	tagInfo, err := regClient.TagMetadata(manifest, &options.ManifestOptions{})
	if err != nil {
		log.Errorf("Could not fetch metadata for %s:%s - no metadata returned by registry: %v", regPrefix, tagStr, err)
		return nil, err
	}
	log.Debugf("Fetched tag metadata for %s:%s", regPrefix, tagStr)

	return tagInfo, nil
}

func getWebhookSecretByPrefix(regPrefix string) string {
	rep, err := registry.GetRegistryEndpoint(regPrefix)
	if err != nil {
		log.Errorf("Could not get registry endpoint %s", regPrefix)
		return ""
	}
	return rep.HookSecret
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	log.Debugf("Received webhook request: %s %s", r.Method, r.URL.Path)

	var webhookEv *WebhookEvent
	var err error

	parts := strings.Split(r.URL.Path, "/")
	regPrefix := parts[3]
	log.Debugf("Registry prefix from URL: %s", regPrefix)

	hookSecret := getWebhookSecretByPrefix(regPrefix)
	log.Debugf("Got webhook secret for %s", regPrefix)

	switch {
	case r.Header.Get("X-Docker-Event") != "":
		log.Debugf("Callback from Dockerhub, X-Docker-Event=%s", r.Header.Get("X-Docker-Event"))
		dockerWebhook := NewDockerWebhook("")
		webhookEv, err = dockerWebhook.Parse(r, (Event(docker.BuildEvent)))
		if err != nil {
			log.Errorf("Could not parse payload: %v", err)
			http.Error(w, "Failed to parse webhook payload", http.StatusBadRequest)
			return
		}
	case r.Header.Get("X-Nexus-Webhook-Id") != "":
		webhookID := r.Header.Get("X-Nexus-Webhook-Id")
		log.Debugf("Callback from Nexus, X-Nexus-Webhook-Id=%s", webhookID)
		if webhookID != string(RepositoryComponentEvent) {
			log.Debugf("Expecting X-Nexus-Webhook-Id header to be %s, got %s", RepositoryComponentEvent, webhookID)
			return
		}
		nexusHook := NewNexusWebhook(hookSecret)
		webhookEv, err = nexusHook.Parse(r, RepositoryComponentEvent)
		if err != nil {
			log.Errorf("Could not parse Nexus payload %v", err)
		}
	default:
		log.Debugf("Ignoring unknown webhook event")
		http.Error(w, "Unknown webhook event", http.StatusBadRequest)
		return
	}

	if err != nil {
		log.Infof("Webhook processing failed: %s", err)
		status := http.StatusBadRequest
		if r.Method != "POST" {
			status = http.StatusMethodNotAllowed
		}
		http.Error(w, fmt.Sprintf("Webhook processing failed: %s", html.EscapeString(err.Error())), status)
		return
	}

	log.Debugf("Parsed webhook event: %+v", webhookEv)

	webhookEv.RegistryPrefix = regPrefix

	tagInfo, err := getTagMetadata(regPrefix, webhookEv.RepoName, webhookEv.TagName)

	if err != nil {
		log.Errorf("Could not get tag metadata for %s:%s. Stop updating: %v", webhookEv.ImageName, webhookEv.TagName, err)
		return
	}
	// webhookEv.Digest = string(tagInfo.Digest[:])
	webhookEv.CreatedAt = tagInfo.CreatedAt

	log.Debugf("HandleEvent: imageName=%s, repoName=%s, tag id=%s, digest=%s, createdAt=%v",
		webhookEv.ImageName, webhookEv.RepoName, webhookEv.TagName, webhookEv.Digest, webhookEv.CreatedAt)

	webhookEventCh <- *webhookEv
	log.Debugf("Sent webhook event to channel")
}
