package ovh

import (
	"encoding/json"
	"fmt"

	"github.com/opencost/opencost/core/pkg/opencost"
	"github.com/opencost/opencost/pkg/cloud"
)

// CloudCostConfiguration represents the configuration for OVH Cloud Cost integration
type CloudCostConfiguration struct {
	ProjectID  string     `json:"projectID"`
	Subsidiary string     `json:"subsidiary"`
	Authorizer Authorizer `json:"authorizer"`
}

func (c *CloudCostConfiguration) Validate() error {
	if c.Authorizer == nil {
		return fmt.Errorf("CloudCostConfiguration: missing Authorizer")
	}

	if err := c.Authorizer.Validate(); err != nil {
		return fmt.Errorf("CloudCostConfiguration: %s", err)
	}

	if c.ProjectID == "" {
		return fmt.Errorf("CloudCostConfiguration: missing projectID")
	}

	return nil
}

func (c *CloudCostConfiguration) Equals(config cloud.Config) bool {
	if config == nil {
		return false
	}
	that, ok := config.(*CloudCostConfiguration)
	if !ok {
		return false
	}

	if c.Authorizer != nil {
		if !c.Authorizer.Equals(that.Authorizer) {
			return false
		}
	} else {
		if that.Authorizer != nil {
			return false
		}
	}

	if c.ProjectID != that.ProjectID {
		return false
	}

	if c.Subsidiary != that.Subsidiary {
		return false
	}

	return true
}

func (c *CloudCostConfiguration) Sanitize() cloud.Config {
	return &CloudCostConfiguration{
		ProjectID:  c.ProjectID,
		Subsidiary: c.Subsidiary,
		Authorizer: c.Authorizer.Sanitize().(Authorizer),
	}
}

func (c *CloudCostConfiguration) Key() string {
	return c.ProjectID
}

func (c *CloudCostConfiguration) Provider() string {
	return opencost.OVHProvider
}

func (c *CloudCostConfiguration) UnmarshalJSON(b []byte) error {
	var f interface{}
	err := json.Unmarshal(b, &f)
	if err != nil {
		return err
	}

	fmap := f.(map[string]interface{})

	projectID, err := cloud.GetInterfaceValue[string](fmap, "projectID")
	if err != nil {
		return fmt.Errorf("CloudCostConfiguration: UnmarshalJSON: %w", err)
	}
	c.ProjectID = projectID

	// Subsidiary is optional, defaults to FR
	if subsidiary, ok := fmap["subsidiary"].(string); ok {
		c.Subsidiary = subsidiary
	} else {
		c.Subsidiary = "FR"
	}

	authAny, ok := fmap["authorizer"]
	if !ok {
		return fmt.Errorf("CloudCostConfiguration: UnmarshalJSON: missing authorizer")
	}
	authorizer, err := cloud.AuthorizerFromInterface(authAny, SelectAuthorizerByType)
	if err != nil {
		return fmt.Errorf("CloudCostConfiguration: UnmarshalJSON: %w", err)
	}
	c.Authorizer = authorizer

	return nil
}
