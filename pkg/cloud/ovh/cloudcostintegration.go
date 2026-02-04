package ovh

import (
	"fmt"
	"strings"
	"time"

	"github.com/opencost/opencost/core/pkg/log"
	"github.com/opencost/opencost/core/pkg/opencost"
	"github.com/opencost/opencost/pkg/cloud"
)

// CloudCostIntegration implements the CloudCostIntegration interface for OVH
type CloudCostIntegration struct {
	CloudCostConfiguration
	ConnectionStatus cloud.ConnectionStatus
}

// UsageResponse represents the OVH usage API response
type UsageResponse struct {
	HourlyUsage struct {
		Instance []struct {
			Reference      string `json:"reference"`
			Region         string `json:"region"`
			DeploymentMode string `json:"deploymentMode"`
			Quantity       struct {
				Unit  string `json:"unit"`
				Value int    `json:"value"`
			} `json:"quantity"`
			TotalPrice float64 `json:"totalPrice"`
			Details    []struct {
				InstanceID string `json:"instanceId"`
				ResourceID string `json:"resourceId"`
				Quantity   struct {
					Unit  string `json:"unit"`
					Value int    `json:"value"`
				} `json:"quantity"`
				TotalPrice float64 `json:"totalPrice"`
			} `json:"details"`
		} `json:"instance"`
		Volume []struct {
			Region     string  `json:"region"`
			Type       string  `json:"type"`
			TotalPrice float64 `json:"totalPrice"`
			Details    []struct {
				VolumeID   string  `json:"volumeId"`
				TotalPrice float64 `json:"totalPrice"`
			} `json:"details"`
		} `json:"volume"`
		Snapshot []struct {
			Region     string  `json:"region"`
			TotalPrice float64 `json:"totalPrice"`
		} `json:"snapshot"`
		Storage []struct {
			Region     string  `json:"region"`
			Type       string  `json:"type"`
			TotalPrice float64 `json:"totalPrice"`
		} `json:"storage"`
		ManagedKubernetesService []struct {
			Reference      string `json:"reference"`
			Region         string `json:"region"`
			DeploymentMode string `json:"deploymentMode"`
			Quantity       struct {
				Value int `json:"value"`
			} `json:"quantity"`
			TotalPrice struct {
				Value float64 `json:"value"`
			} `json:"totalPrice"`
			Details []struct {
				ID         string `json:"id"`
				ResourceID string `json:"resourceId"`
				Quantity   struct {
					Value int `json:"value"`
				} `json:"quantity"`
				TotalPrice struct {
					Value float64 `json:"value"`
				} `json:"totalPrice"`
			} `json:"details"`
		} `json:"managedKubernetesService"`
		Rancher []struct {
			Reference  string `json:"reference"`
			TotalPrice struct {
				Value float64 `json:"value"`
			} `json:"totalPrice"`
			Details []struct {
				RancherID  string `json:"rancherId"`
				ResourceID string `json:"resourceId"`
				Quantity   struct {
					Value int `json:"value"`
				} `json:"quantity"`
				TotalPrice struct {
					Value float64 `json:"value"`
				} `json:"totalPrice"`
			} `json:"details"`
		} `json:"rancher"`
	} `json:"hourlyUsage"`
	ResourcesUsage []struct {
		Type      string `json:"type"`
		Resources []struct {
			Components []struct {
				ID         string `json:"id"`
				Name       string `json:"name"`
				ResourceID string `json:"resourceId"`
				Quantity   struct {
					Unit  string `json:"unit"`
					Value int    `json:"value"`
				} `json:"quantity"`
				TotalPrice float64 `json:"totalPrice"`
			} `json:"components"`
		} `json:"resources"`
	} `json:"resourcesUsage"`
	Period struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"period"`
}

// HistoryPeriod represents a billing period from the /usage/history endpoint
type HistoryPeriod struct {
	ID     string `json:"id"`
	Period struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"period"`
}

func (cci *CloudCostIntegration) GetCloudCost(start time.Time, end time.Time) (*opencost.CloudCostSetRange, error) {
	client, err := cci.Authorizer.CreateOVHClient()
	if err != nil {
		cci.ConnectionStatus = cloud.FailedConnection
		return nil, fmt.Errorf("getting OVH client: %w", err)
	}

	ccsr, err := opencost.NewCloudCostSetRange(start, end, opencost.AccumulateOptionDay, cci.Key())
	if err != nil {
		return nil, err
	}

	dataLoaded := false

	// 1. Fetch current month usage
	var currentUsage UsageResponse
	err = client.Get(fmt.Sprintf("/cloud/project/%s/usage/current", cci.ProjectID), &currentUsage)
	if err != nil {
		log.Warnf("OVH Cloud Cost: failed to fetch current usage: %v", err)
	} else {
		loaded := cci.processUsagePeriod(&currentUsage, ccsr, start, end)
		dataLoaded = dataLoaded || loaded
	}

	// 2. Fetch historical usage for previous months
	var historyPeriods []HistoryPeriod
	err = client.Get(fmt.Sprintf("/cloud/project/%s/usage/history", cci.ProjectID), &historyPeriods)
	if err != nil {
		log.Warnf("OVH Cloud Cost: failed to fetch usage history: %v", err)
	} else {
		log.Infof("OVH Cloud Cost: found %d historical billing periods", len(historyPeriods))
		for _, period := range historyPeriods {
			// Parse period dates to check if it overlaps with our window
			periodStart, err := cci.parseOVHDate(period.Period.From)
			if err != nil {
				continue
			}

			// Only fetch detailed data if period might overlap with our window
			if !periodStart.Before(end) || periodStart.AddDate(0, 1, 0).Before(start) {
				continue
			}

			// Fetch detailed usage for this historical period
			var historicalUsage UsageResponse
			err = client.Get(fmt.Sprintf("/cloud/project/%s/usage/%s", cci.ProjectID, period.ID), &historicalUsage)
			if err != nil {
				log.Warnf("OVH Cloud Cost: failed to fetch historical usage %s: %v", period.ID, err)
				continue
			}

			loaded := cci.processUsagePeriod(&historicalUsage, ccsr, start, end)
			dataLoaded = dataLoaded || loaded
		}
	}

	if !dataLoaded {
		cci.ConnectionStatus = cloud.MissingData
	} else {
		cci.ConnectionStatus = cloud.SuccessfulConnection
	}

	return ccsr, nil
}

// parseOVHDate parses dates from OVH API responses
func (cci *CloudCostIntegration) parseOVHDate(dateStr string) (time.Time, error) {
	// Try RFC3339 first
	t, err := time.Parse(time.RFC3339, dateStr)
	if err == nil {
		return t, nil
	}
	// Try alternative format
	t, err = time.Parse("2006-01-02T15:04:05.000Z", dateStr)
	if err == nil {
		return t, nil
	}
	// Try date only
	t, err = time.Parse("2006-01-02", dateStr)
	if err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("failed to parse date: %s", dateStr)
}

// processUsagePeriod processes usage data for a single billing period
func (cci *CloudCostIntegration) processUsagePeriod(usage *UsageResponse, ccsr *opencost.CloudCostSetRange, start, end time.Time) bool {
	log.Infof("OVH Cloud Cost: processing usage data for period %s to %s", usage.Period.From, usage.Period.To)
	log.Infof("OVH Cloud Cost: instances=%d, volumes=%d, snapshots=%d, storage=%d, mks=%d, rancher=%d, resourcesUsage=%d",
		len(usage.HourlyUsage.Instance),
		len(usage.HourlyUsage.Volume),
		len(usage.HourlyUsage.Snapshot),
		len(usage.HourlyUsage.Storage),
		len(usage.HourlyUsage.ManagedKubernetesService),
		len(usage.HourlyUsage.Rancher),
		len(usage.ResourcesUsage),
	)

	// Parse the billing period start date
	billingStart, err := cci.parseOVHDate(usage.Period.From)
	if err != nil {
		log.Warnf("OVH Cloud Cost: failed to parse billing period start date %s: %v", usage.Period.From, err)
		return false
	}

	// Only load costs if the requested window includes the billing period start date
	if billingStart.Before(start) || !billingStart.Before(end) {
		log.Debugf("OVH Cloud Cost: skipping period starting %s - outside window [%s, %s)",
			billingStart.Format(time.RFC3339), start.Format(time.RFC3339), end.Format(time.RFC3339))
		return false
	}

	log.Infof("OVH Cloud Cost: loading data for billing period starting %s", billingStart.Format(time.RFC3339))

	// Use the billing period start date for all costs
	costDate := billingStart

	// Process instances (compute)
	for _, instance := range usage.HourlyUsage.Instance {
		log.Debugf("OVH Cloud Cost: processing instance %s (region=%s, price=%f, details=%d)",
			instance.Reference, instance.Region, instance.TotalPrice, len(instance.Details))
		for _, detail := range instance.Details {
			cc := cci.createCloudCost(
				detail.ResourceID,
				"Compute",
				instance.Reference,
				opencost.ComputeCategory,
				instance.Region,
				detail.TotalPrice,
				costDate,
			)
			ccsr.LoadCloudCost(cc)
		}
	}

	// Process volumes (storage)
	for _, volume := range usage.HourlyUsage.Volume {
		for _, detail := range volume.Details {
			cc := cci.createCloudCost(
				detail.VolumeID,
				"Block Storage",
				volume.Type,
				opencost.StorageCategory,
				volume.Region,
				detail.TotalPrice,
				costDate,
			)
			ccsr.LoadCloudCost(cc)
		}
	}

	// Process snapshots
	for _, snapshot := range usage.HourlyUsage.Snapshot {
		cc := cci.createCloudCost(
			"snapshot-"+snapshot.Region,
			"Snapshot",
			"snapshot",
			opencost.StorageCategory,
			snapshot.Region,
			snapshot.TotalPrice,
			costDate,
		)
		ccsr.LoadCloudCost(cc)
	}

	// Process object storage
	for _, storage := range usage.HourlyUsage.Storage {
		cc := cci.createCloudCost(
			"storage-"+storage.Region,
			"Object Storage",
			storage.Type,
			opencost.StorageCategory,
			storage.Region,
			storage.TotalPrice,
			costDate,
		)
		ccsr.LoadCloudCost(cc)
	}

	// Process MKS (Managed Kubernetes Service)
	for _, mks := range usage.HourlyUsage.ManagedKubernetesService {
		log.Debugf("OVH Cloud Cost: processing MKS %s (region=%s, price=%f, details=%d)",
			mks.Reference, mks.Region, mks.TotalPrice.Value, len(mks.Details))
		for _, detail := range mks.Details {
			cc := cci.createCloudCost(
				detail.ResourceID,
				"Managed Kubernetes Service",
				mks.Reference,
				opencost.ManagementCategory,
				mks.Region,
				detail.TotalPrice.Value,
				costDate,
			)
			ccsr.LoadCloudCost(cc)
		}
	}

	// Process Rancher
	for _, rancher := range usage.HourlyUsage.Rancher {
		for _, detail := range rancher.Details {
			cc := cci.createCloudCost(
				detail.ResourceID,
				"Rancher",
				rancher.Reference,
				opencost.ManagementCategory,
				"",
				detail.TotalPrice.Value,
				costDate,
			)
			ccsr.LoadCloudCost(cc)
		}
	}

	// Process resourcesUsage for additional services
	cci.processResourcesUsageData(usage, ccsr, costDate)

	// Check if we got any data
	hasData := len(usage.HourlyUsage.Instance) > 0 ||
		len(usage.HourlyUsage.Volume) > 0 ||
		len(usage.HourlyUsage.Snapshot) > 0 ||
		len(usage.HourlyUsage.Storage) > 0 ||
		len(usage.HourlyUsage.ManagedKubernetesService) > 0 ||
		len(usage.HourlyUsage.Rancher) > 0 ||
		len(usage.ResourcesUsage) > 0

	return hasData
}

func (cci *CloudCostIntegration) processResourcesUsageData(usage *UsageResponse, ccsr *opencost.CloudCostSetRange, costDate time.Time) {
	for _, resourceGroup := range usage.ResourcesUsage {
		for _, resource := range resourceGroup.Resources {
			for _, component := range resource.Components {
				service, category := cci.categorizeResource(component.Name)
				cc := cci.createCloudCost(
					component.ResourceID,
					service,
					component.Name,
					category,
					"",
					component.TotalPrice,
					costDate,
				)
				ccsr.LoadCloudCost(cc)
			}
		}
	}
}

func (cci *CloudCostIntegration) categorizeResource(name string) (string, string) {
	nameLower := strings.ToLower(name)

	// Databases
	if strings.Contains(nameLower, "postgresql") {
		return "Database - PostgreSQL", opencost.OtherCategory
	}
	if strings.Contains(nameLower, "mysql") {
		return "Database - MySQL", opencost.OtherCategory
	}
	if strings.Contains(nameLower, "mongodb") {
		return "Database - MongoDB", opencost.OtherCategory
	}
	if strings.Contains(nameLower, "redis") || strings.Contains(nameLower, "valkey") {
		return "Database - Redis/Valkey", opencost.OtherCategory
	}
	if strings.Contains(nameLower, "kafka") {
		return "Database - Kafka", opencost.OtherCategory
	}
	if strings.Contains(nameLower, "cassandra") {
		return "Database - Cassandra", opencost.OtherCategory
	}
	if strings.Contains(nameLower, "opensearch") {
		return "Database - OpenSearch", opencost.OtherCategory
	}
	if strings.Contains(nameLower, "grafana") {
		return "Database - Grafana", opencost.OtherCategory
	}
	if strings.Contains(nameLower, "m3db") {
		return "Database - M3DB", opencost.OtherCategory
	}

	// AI Services
	if strings.Contains(nameLower, "ai-") || strings.Contains(nameLower, "ai1-") {
		return "AI Services", opencost.ComputeCategory
	}
	if strings.Contains(nameLower, "notebook") {
		return "AI Notebook", opencost.ComputeCategory
	}
	if strings.Contains(nameLower, "training") {
		return "AI Training", opencost.ComputeCategory
	}

	// Network
	if strings.Contains(nameLower, "loadbalancer") {
		return "Load Balancer", opencost.NetworkCategory
	}
	if strings.Contains(nameLower, "gateway") {
		return "Gateway", opencost.NetworkCategory
	}
	if strings.Contains(nameLower, "floatingip") {
		return "Floating IP", opencost.NetworkCategory
	}
	if strings.Contains(nameLower, "ip") {
		return "Public IP", opencost.NetworkCategory
	}

	// Registry
	if strings.Contains(nameLower, "registry") || strings.Contains(nameLower, "plan-equivalent") {
		return "Container Registry", opencost.StorageCategory
	}

	// Storage
	if strings.Contains(nameLower, "archive") {
		return "Cold Archive", opencost.StorageCategory
	}
	if strings.Contains(nameLower, "storage") {
		return "Object Storage", opencost.StorageCategory
	}

	// Data Platform
	if strings.Contains(nameLower, "dataplatform") {
		return "Data Platform", opencost.OtherCategory
	}

	// Default
	return "Other", opencost.OtherCategory
}

func (cci *CloudCostIntegration) createCloudCost(
	resourceID string,
	service string,
	productName string,
	category string,
	region string,
	cost float64,
	windowStart time.Time,
) *opencost.CloudCost {
	windowEnd := windowStart.AddDate(0, 0, 1)

	labels := opencost.CloudCostLabels{
		"product": productName,
	}

	properties := &opencost.CloudCostProperties{
		ProviderID:      resourceID,
		Provider:        opencost.OVHProvider,
		AccountID:       cci.ProjectID,
		AccountName:     cci.ProjectID,
		InvoiceEntityID: cci.ProjectID,
		RegionID:        region,
		Service:         service,
		Category:        category,
		Labels:          labels,
	}

	return &opencost.CloudCost{
		Properties: properties,
		Window:     opencost.NewWindow(&windowStart, &windowEnd),
		ListCost: opencost.CostMetric{
			Cost: cost,
		},
		NetCost: opencost.CostMetric{
			Cost: cost,
		},
		AmortizedNetCost: opencost.CostMetric{
			Cost: cost,
		},
		AmortizedCost: opencost.CostMetric{
			Cost: cost,
		},
		InvoicedCost: opencost.CostMetric{
			Cost: cost,
		},
	}
}

func (cci *CloudCostIntegration) GetStatus() cloud.ConnectionStatus {
	if cci.ConnectionStatus.String() == "" {
		cci.ConnectionStatus = cloud.InitialStatus
	}
	return cci.ConnectionStatus
}

func (cci *CloudCostIntegration) RefreshStatus() cloud.ConnectionStatus {
	client, err := cci.Authorizer.CreateOVHClient()
	if err != nil {
		cci.ConnectionStatus = cloud.FailedConnection
		log.Warnf("OVH Cloud Cost: failed to create client: %v", err)
		return cci.ConnectionStatus
	}

	// Try a simple API call to verify connection
	var project struct {
		ProjectID string `json:"project_id"`
	}
	err = client.Get(fmt.Sprintf("/cloud/project/%s", cci.ProjectID), &project)
	if err != nil {
		cci.ConnectionStatus = cloud.FailedConnection
		log.Warnf("OVH Cloud Cost: failed to validate connection: %v", err)
		return cci.ConnectionStatus
	}

	cci.ConnectionStatus = cloud.SuccessfulConnection
	return cci.ConnectionStatus
}
