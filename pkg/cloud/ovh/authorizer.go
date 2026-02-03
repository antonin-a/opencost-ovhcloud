package ovh

import (
	"fmt"

	"github.com/ovh/go-ovh/ovh"

	"github.com/opencost/opencost/core/pkg/util/json"
	"github.com/opencost/opencost/pkg/cloud"
)

const (
	OVHAuthorizerTypeAPIKey = "OVHAPIKey"
)

// Authorizer provides an interface for OVH API authentication
type Authorizer interface {
	cloud.Authorizer
	CreateOVHClient() (*ovh.Client, error)
}

// APIKeyAuthorizer authenticates using OVH API key credentials
type APIKeyAuthorizer struct {
	Endpoint          string `json:"endpoint"`
	ApplicationKey    string `json:"applicationKey"`
	ApplicationSecret string `json:"applicationSecret"`
	ConsumerKey       string `json:"consumerKey"`
}

func (aka *APIKeyAuthorizer) Validate() error {
	if aka.ApplicationKey == "" {
		return fmt.Errorf("APIKeyAuthorizer: missing applicationKey")
	}
	if aka.ApplicationSecret == "" {
		return fmt.Errorf("APIKeyAuthorizer: missing applicationSecret")
	}
	if aka.ConsumerKey == "" {
		return fmt.Errorf("APIKeyAuthorizer: missing consumerKey")
	}
	return nil
}

func (aka *APIKeyAuthorizer) Equals(config cloud.Config) bool {
	if config == nil {
		return false
	}
	that, ok := config.(*APIKeyAuthorizer)
	if !ok {
		return false
	}
	return aka.Endpoint == that.Endpoint &&
		aka.ApplicationKey == that.ApplicationKey &&
		aka.ApplicationSecret == that.ApplicationSecret &&
		aka.ConsumerKey == that.ConsumerKey
}

func (aka *APIKeyAuthorizer) Sanitize() cloud.Config {
	return &APIKeyAuthorizer{
		Endpoint:          aka.Endpoint,
		ApplicationKey:    aka.ApplicationKey,
		ApplicationSecret: cloud.Redacted,
		ConsumerKey:       cloud.Redacted,
	}
}

func (aka *APIKeyAuthorizer) CreateOVHClient() (*ovh.Client, error) {
	endpoint := aka.Endpoint
	if endpoint == "" {
		endpoint = "ovh-eu"
	}

	client, err := ovh.NewClient(endpoint, aka.ApplicationKey, aka.ApplicationSecret, aka.ConsumerKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create OVH client: %w", err)
	}
	return client, nil
}

// MarshalJSON implements the json.Marshaler interface
func (aka *APIKeyAuthorizer) MarshalJSON() ([]byte, error) {
	fmap := make(map[string]interface{})
	fmap[cloud.AuthorizerTypeProperty] = OVHAuthorizerTypeAPIKey
	fmap["endpoint"] = aka.Endpoint
	fmap["applicationKey"] = aka.ApplicationKey
	fmap["applicationSecret"] = aka.ApplicationSecret
	fmap["consumerKey"] = aka.ConsumerKey
	return json.Marshal(fmap)
}

// SelectAuthorizerByType returns the appropriate Authorizer based on type
func SelectAuthorizerByType(typeStr string) (Authorizer, error) {
	switch typeStr {
	case OVHAuthorizerTypeAPIKey:
		return &APIKeyAuthorizer{}, nil
	default:
		return nil, fmt.Errorf("OVH: invalid MSP Authorizer type: %s", typeStr)
	}
}
