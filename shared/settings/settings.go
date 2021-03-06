package settings

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/argoproj-labs/argocd-notifications/notifiers"
	"github.com/argoproj-labs/argocd-notifications/shared/argocd"
	"github.com/argoproj-labs/argocd-notifications/triggers"
	log "github.com/sirupsen/logrus"

	"github.com/ghodss/yaml"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

type rawSubscription struct {
	Recipients []string
	Triggers   []string
	Selector   string
}

// DefaultSubscription holds recipients that receives notification by default.
type Subscription struct {
	// Recipients comma separated list of recipients
	Recipients []string
	// Optional trigger name
	Triggers []string
	// Options label selector that limits applied applications
	Selector labels.Selector
}

func (s *Subscription) MatchesTrigger(trigger string) bool {
	if len(s.Triggers) == 0 {
		return true
	}
	for i := range s.Triggers {
		if s.Triggers[i] == trigger {
			return true
		}
	}
	return false
}

func (s *Subscription) UnmarshalJSON(data []byte) error {
	raw := rawSubscription{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Triggers = raw.Triggers
	s.Recipients = raw.Recipients
	selector, err := labels.Parse(raw.Selector)
	if err != nil {
		return err
	}
	s.Selector = selector
	return nil
}

func (s *Subscription) MarshalJSON() ([]byte, error) {
	raw := rawSubscription{
		Triggers:   s.Triggers,
		Recipients: s.Recipients,
	}
	if s.Selector != nil {
		raw.Selector = s.Selector.String()
	}
	return json.Marshal(raw)
}

type DefaultSubscriptions []Subscription

// Returns list of recipients for the specified trigger
func (subscriptions DefaultSubscriptions) GetRecipients(trigger string, labels map[string]string) []string {
	var result []string
	for _, s := range subscriptions {
		if s.MatchesTrigger(trigger) && s.Selector.Matches(fields.Set(labels)) {
			result = append(result, s.Recipients...)
		}
	}
	return result
}

type Config struct {
	Triggers      []triggers.NotificationTrigger  `json:"triggers,omitempty" patchStrategy:"merge" patchMergeKey:"name"`
	Templates     []triggers.NotificationTemplate `json:"templates,omitempty" patchStrategy:"merge" patchMergeKey:"name"`
	Context       map[string]string               `json:"context,omitempty"`
	Subscriptions DefaultSubscriptions            `json:"subscriptions,omitempty"`
}

// ParseSecret retrieves configured notification services from the provided secret
func ParseSecret(secret *v1.Secret) (notifiersConfig notifiers.Config, err error) {
	notifiersData := secret.Data["notifiers.yaml"]
	err = yaml.Unmarshal(notifiersData, &notifiersConfig)
	if err != nil {
		return notifiers.Config{}, err
	}
	return notifiersConfig, nil
}

// ParseSecret retrieves configured templates and triggers from the provided config map
func ParseConfigMap(configMap *v1.ConfigMap) (*Config, error) {
	root := &Config{}
	cfg := &Config{}
	// read all the keys in format of templates.%s and triggers.%s
	// to create config
	for k, v := range configMap.Data {
		if k == "config.yaml" {
			// config.yaml should be read at the end to create base templates and triggers
			continue
		}
		parts := strings.Split(k, ".")
		if strings.HasPrefix(k, "template") {
			name := strings.Join(parts[1:], ".")
			tmpl := triggers.NotificationTemplate{}
			if err := yaml.Unmarshal([]byte(v), &tmpl); err != nil {
				return root, fmt.Errorf("Failed to unmarshal template %s: %v", name, err)
			}
			tmpl.Name = name
			root.Templates = append(root.Templates, tmpl)
			continue
		}

		if strings.HasPrefix(k, "trigger") {
			name := strings.Join(parts[1:], ".")
			trigger := triggers.NotificationTrigger{}
			if err := yaml.Unmarshal([]byte(v), &trigger); err != nil {
				return root, fmt.Errorf("Failed to unmarshal trigger %s: %v", name, err)
			}
			trigger.Name = name
			root.Triggers = append(root.Triggers, trigger)
			continue

		}

		log.Infof("Key %s does not match to pattern, ignored", k)
		continue

	}
	if data, ok := configMap.Data["config.yaml"]; ok {
		err := yaml.Unmarshal([]byte(data), &cfg)
		if err != nil {
			return cfg, fmt.Errorf("Failed to read config.yaml key from configmap: %v", err)
		}
	}
	return cfg.Merge(root)
}

func (cfg *Config) Merge(other *Config) (*Config, error) {
	origData, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	otherData, err := json.Marshal(other)
	if err != nil {
		return nil, err
	}

	mergedData, err := strategicpatch.StrategicMergePatch(origData, otherData, &Config{})
	if err != nil {
		return nil, err
	}

	res := &Config{}
	err = json.Unmarshal(mergedData, res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ParseConfig parses notifications configuration from the provided config map and secret.
func ParseConfig(configMap *v1.ConfigMap, secret *v1.Secret, defaultCfg Config, argocdService argocd.Service) (map[string]triggers.Trigger, map[string]notifiers.Notifier, *Config, error) {
	cfg, err := ParseConfigMap(configMap)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err = defaultCfg.Merge(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	t, err := triggers.GetTriggers(cfg.Templates, cfg.Triggers, argocdService)
	if err != nil {
		return nil, nil, nil, err
	}
	notifiersConfig, err := ParseSecret(secret)
	if err != nil {
		return nil, nil, nil, err
	}
	return t, notifiers.GetAll(notifiersConfig), cfg, nil
}
