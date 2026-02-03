package ovh

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/ovh/go-ovh/ovh"

	"github.com/opencost/opencost/core/pkg/clustercache"
	coreenv "github.com/opencost/opencost/core/pkg/env"
	"github.com/opencost/opencost/core/pkg/log"
	"github.com/opencost/opencost/core/pkg/opencost"
	"github.com/opencost/opencost/core/pkg/util"
	"github.com/opencost/opencost/core/pkg/util/json"
	"github.com/opencost/opencost/pkg/cloud/models"
	"github.com/opencost/opencost/pkg/cloud/utils"
	"github.com/opencost/opencost/pkg/env"
)

const (
	OVHCatalogPricing = "OVH Catalog Pricing"
	OVHAPIPricing     = "OVH API Pricing"

	// Public catalog URL (no auth required)
	CatalogURLTemplate = "https://api.ovh.com/1.0/order/catalog/public/cloud?ovhSubsidiary=%s"
)

// GPUTypes maps OVH GPU instance prefixes to GPU names
var GPUTypes = map[string]string{
	"a10":  "NVIDIA A10",
	"a100": "NVIDIA A100",
	"l4":   "NVIDIA L4",
	"l40s": "NVIDIA L40S",
	"h100": "NVIDIA H100",
}

// OVHFlavor represents an OVH instance flavor from API
type OVHFlavor struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Region    string `json:"region"`
	VCPUs     int    `json:"vcpus"`
	RAM       int    `json:"ram"`  // in MB
	Disk      int    `json:"disk"` // in GB
	Type      string `json:"type"`
	Available bool   `json:"available"`
	PlanCodes struct {
		Hourly  string `json:"hourly"`
		Monthly string `json:"monthly"`
	} `json:"planCodes"`
}

// OVHPricing holds all pricing data
type OVHPricing struct {
	// Catalog prices by plan code (from public catalog)
	CatalogPrices map[string]float64 // planCode -> EUR/hour

	// Flavor specs by region and name (from authenticated API)
	Flavors map[string]map[string]*OVHFlavor // region -> flavorName -> info

	// Volume pricing
	VolumePrices map[string]float64 // volumeType -> EUR/GB/hour
}

// OVH implements the Provider interface for OVHcloud
type OVH struct {
	Clientset               clustercache.ClusterCache
	Config                  models.ProviderConfig
	Pricing                 *OVHPricing
	ClusterRegion           string
	ClusterAccountID        string
	DownloadPricingDataLock sync.RWMutex

	// OVH API client
	client *ovh.Client

	// Track pricing source availability
	catalogAvailable bool
	apiAvailable     bool
}

// PricingSourceSummary returns the pricing source summary for the provider.
func (o *OVH) PricingSourceSummary() interface{} {
	return o.Pricing
}

// NewOVHClient creates a new authenticated OVH API client
func NewOVHClient() (*ovh.Client, error) {
	appKey := env.GetOVHApplicationKey()
	appSecret := env.GetOVHApplicationSecret()
	consumerKey := env.GetOVHConsumerKey()
	endpoint := env.GetOVHEndpoint()

	if appKey == "" || appSecret == "" || consumerKey == "" {
		return nil, fmt.Errorf("OVH credentials not configured: OVH_APPLICATION_KEY, OVH_APPLICATION_SECRET, OVH_CONSUMER_KEY required")
	}

	return ovh.NewClient(endpoint, appKey, appSecret, consumerKey)
}

// DownloadPricingData fetches pricing data from OVH catalog and API
func (o *OVH) DownloadPricingData() error {
	o.DownloadPricingDataLock.Lock()
	defer o.DownloadPricingDataLock.Unlock()

	o.Pricing = &OVHPricing{
		CatalogPrices: make(map[string]float64),
		Flavors:       make(map[string]map[string]*OVHFlavor),
		VolumePrices:  make(map[string]float64),
	}

	// 1. Fetch public catalog for static pricing
	if err := o.fetchCatalogPricing(); err != nil {
		log.Warnf("Failed to fetch OVH catalog: %v", err)
	}

	// 2. Fetch flavor specs via authenticated API (MANDATORY)
	if err := o.fetchFlavorSpecs(); err != nil {
		log.Errorf("Failed to fetch OVH flavors: %v", err)
		return err
	}

	// 3. Set volume pricing from catalog
	o.setVolumePricing()

	return nil
}

func (o *OVH) fetchCatalogPricing() error {
	subsidiary := env.GetOVHSubsidiary()
	catalogURL := fmt.Sprintf(CatalogURLTemplate, subsidiary)

	resp, err := http.Get(catalogURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var catalog struct {
		Addons []struct {
			PlanCode string `json:"planCode"`
			Product  string `json:"product"`
			Pricings []struct {
				Price        int64    `json:"price"`
				IntervalUnit string   `json:"intervalUnit"`
				Capacities   []string `json:"capacities"`
			} `json:"pricings"`
		} `json:"addons"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return err
	}

	for _, addon := range catalog.Addons {
		// Instance pricing
		if addon.Product == "publiccloud-instance" && strings.Contains(addon.PlanCode, ".consumption") {
			for _, pricing := range addon.Pricings {
				if contains(pricing.Capacities, "consumption") {
					// Convert micro-cents to EUR
					priceEUR := float64(pricing.Price) / 100000000.0
					o.Pricing.CatalogPrices[addon.PlanCode] = priceEUR
				}
			}
		}

		// Volume pricing
		if strings.HasPrefix(addon.Product, "publiccloud-volume") {
			for _, pricing := range addon.Pricings {
				if contains(pricing.Capacities, "consumption") {
					priceEUR := float64(pricing.Price) / 100000000.0
					o.Pricing.CatalogPrices[addon.PlanCode] = priceEUR
				}
			}
		}

		// Snapshot pricing
		if addon.Product == "publiccloud-snapshot" {
			for _, pricing := range addon.Pricings {
				if contains(pricing.Capacities, "consumption") {
					priceEUR := float64(pricing.Price) / 100000000.0
					o.Pricing.CatalogPrices[addon.PlanCode] = priceEUR
				}
			}
		}
	}

	o.catalogAvailable = len(o.Pricing.CatalogPrices) > 0
	log.Infof("Loaded %d prices from OVH catalog", len(o.Pricing.CatalogPrices))
	return nil
}

func (o *OVH) fetchFlavorSpecs() error {
	client, err := NewOVHClient()
	if err != nil {
		return err
	}
	o.client = client

	projectID := env.GetOVHProjectID()
	if projectID == "" {
		return fmt.Errorf("OVH_PROJECT_ID not set")
	}

	var flavors []OVHFlavor
	err = client.Get(fmt.Sprintf("/cloud/project/%s/flavor", projectID), &flavors)
	if err != nil {
		return fmt.Errorf("failed to get OVH flavors: %w", err)
	}

	for _, flavor := range flavors {
		region := strings.ToUpper(flavor.Region)
		if _, ok := o.Pricing.Flavors[region]; !ok {
			o.Pricing.Flavors[region] = make(map[string]*OVHFlavor)
		}
		f := flavor
		o.Pricing.Flavors[region][flavor.Name] = &f
	}

	o.apiAvailable = true
	log.Infof("Loaded %d flavors from OVH API across %d regions", len(flavors), len(o.Pricing.Flavors))
	return nil
}

func (o *OVH) setVolumePricing() {
	// Default volume pricing (EUR per GB per hour)
	o.Pricing.VolumePrices["classic"] = 0.000059
	o.Pricing.VolumePrices["high-speed"] = 0.000119
	o.Pricing.VolumePrices["snapshot"] = 0.000015

	// Override with catalog prices if available
	if price, ok := o.Pricing.CatalogPrices["volume.classic.consumption"]; ok {
		o.Pricing.VolumePrices["classic"] = price
	}
	if price, ok := o.Pricing.CatalogPrices["volume.high-speed.consumption"]; ok {
		o.Pricing.VolumePrices["high-speed"] = price
	}
	if price, ok := o.Pricing.CatalogPrices["snapshot.consumption"]; ok {
		o.Pricing.VolumePrices["snapshot"] = price
	}
}

// AllNodePricing returns all pricing data
func (o *OVH) AllNodePricing() (interface{}, error) {
	o.DownloadPricingDataLock.RLock()
	defer o.DownloadPricingDataLock.RUnlock()
	return o.Pricing, nil
}

// ovhKey implements the Key interface
type ovhKey struct {
	Labels map[string]string
}

func (k *ovhKey) Features() string {
	instanceType, _ := util.GetInstanceType(k.Labels)
	region, _ := util.GetRegion(k.Labels)
	return region + "," + instanceType
}

func (k *ovhKey) GPUCount() int {
	instanceType, _ := util.GetInstanceType(k.Labels)
	// GPU instances have predictable naming: a10-45, a100-180, l4-90, etc.
	for prefix := range GPUTypes {
		if strings.HasPrefix(strings.ToLower(instanceType), prefix) {
			return 1 // OVH GPU instances have 1 GPU per instance
		}
	}
	return 0
}

func (k *ovhKey) GPUType() string {
	instanceType, _ := util.GetInstanceType(k.Labels)
	instanceLower := strings.ToLower(instanceType)

	for prefix, gpuName := range GPUTypes {
		if strings.HasPrefix(instanceLower, prefix) {
			return gpuName
		}
	}
	return ""
}

func (k *ovhKey) ID() string {
	return ""
}

// NodePricing returns the pricing for a node
func (o *OVH) NodePricing(key models.Key) (*models.Node, models.PricingMetadata, error) {
	o.DownloadPricingDataLock.RLock()
	defer o.DownloadPricingDataLock.RUnlock()

	meta := models.PricingMetadata{}
	features := strings.Split(key.Features(), ",")
	if len(features) < 2 {
		return nil, meta, fmt.Errorf("invalid key features: %s", key.Features())
	}

	region := strings.ToUpper(features[0])
	instanceType := features[1]

	// Get flavor from API data
	var flavor *OVHFlavor
	if regionFlavors, ok := o.Pricing.Flavors[region]; ok {
		flavor = regionFlavors[instanceType]
	}

	// Determine plan code and price
	var price float64
	var found bool

	if flavor != nil && flavor.PlanCodes.Hourly != "" {
		price, found = o.Pricing.CatalogPrices[flavor.PlanCodes.Hourly]
	}

	if !found {
		// Fallback: try common plan code patterns
		planCode := o.getPlanCodeForRegion(instanceType, region)
		price, found = o.Pricing.CatalogPrices[planCode]
	}

	if !found {
		// Last fallback: base consumption plan
		price, found = o.Pricing.CatalogPrices[instanceType+".consumption"]
	}

	if !found {
		return nil, meta, fmt.Errorf("no pricing found for %s in region %s", instanceType, region)
	}

	node := &models.Node{
		Cost:         fmt.Sprintf("%f", price),
		PricingType:  models.Api,
		InstanceType: instanceType,
		Region:       region,
	}

	// Fill in flavor specs
	if flavor != nil {
		node.VCPU = fmt.Sprintf("%d", flavor.VCPUs)
		node.RAM = fmt.Sprintf("%d", flavor.RAM) // MB
		node.Storage = fmt.Sprintf("%d", flavor.Disk)
	}

	// GPU detection
	gpuType := key.GPUType()
	if gpuType != "" {
		node.GPUName = gpuType
		node.GPU = fmt.Sprintf("%d", key.GPUCount())
	}

	return node, meta, nil
}

func (o *OVH) getPlanCodeForRegion(flavor, region string) string {
	region = strings.ToUpper(region)

	// 3-AZ regions
	if region == "EU-WEST-PAR" || region == "EU-SOUTH-MIL" {
		return flavor + ".consumption.3AZ"
	}

	// Local Zones
	if strings.Contains(region, "LZ-MRS") {
		return flavor + ".consumption.LZ.EUROZONE"
	}
	if strings.Contains(region, "LZ") {
		return flavor + ".consumption.LZ"
	}

	// Standard regions
	return flavor + ".consumption"
}

// LoadBalancerPricing returns load balancer pricing
func (o *OVH) LoadBalancerPricing() (*models.LoadBalancer, error) {
	// OVH Load Balancer pricing (basic tier ~6 EUR/month)
	return &models.LoadBalancer{
		Cost: 0.008,
	}, nil
}

// NetworkPricing returns network pricing (free for OVH)
func (o *OVH) NetworkPricing() (*models.Network, error) {
	return &models.Network{
		ZoneNetworkEgressCost:     0.0,
		RegionNetworkEgressCost:   0.0,
		InternetNetworkEgressCost: 0.0,
	}, nil
}

// GetKey returns a Key for the given labels and node
func (o *OVH) GetKey(l map[string]string, n *clustercache.Node) models.Key {
	return &ovhKey{Labels: l}
}

// ovhPVKey implements the PVKey interface
type ovhPVKey struct {
	Labels           map[string]string
	StorageClassName string
	Zone             string
	VolumeType       string // classic, high-speed
}

func (k *ovhPVKey) Features() string {
	return k.Zone + "," + k.VolumeType
}

func (k *ovhPVKey) GetStorageClass() string {
	return k.StorageClassName
}

func (k *ovhPVKey) ID() string {
	return ""
}

// GetPVKey returns a PVKey for the given PersistentVolume
func (o *OVH) GetPVKey(pv *clustercache.PersistentVolume, parameters map[string]string, defaultRegion string) models.PVKey {
	zone := ""
	volumeType := "high-speed" // default for OVH CSI

	if pv.Spec.CSI != nil {
		// OVH Cinder CSI volume handle: zone/volume-id
		parts := strings.Split(pv.Spec.CSI.VolumeHandle, "/")
		if len(parts) > 0 {
			zone = parts[0]
		}
		// Check storage class for volume type
		if strings.Contains(strings.ToLower(pv.Spec.StorageClassName), "classic") {
			volumeType = "classic"
		}
	}

	return &ovhPVKey{
		Labels:           pv.Labels,
		StorageClassName: pv.Spec.StorageClassName,
		Zone:             zone,
		VolumeType:       volumeType,
	}
}

// GpuPricing returns GPU pricing information
func (o *OVH) GpuPricing(nodeLabels map[string]string) (string, error) {
	key := &ovhKey{Labels: nodeLabels}
	if gpuType := key.GPUType(); gpuType != "" {
		return gpuType, nil
	}
	return "", nil
}

// PVPricing returns pricing for a PersistentVolume
func (o *OVH) PVPricing(pvk models.PVKey) (*models.PV, error) {
	o.DownloadPricingDataLock.RLock()
	defer o.DownloadPricingDataLock.RUnlock()

	features := strings.Split(pvk.Features(), ",")
	volumeType := "high-speed"
	if len(features) > 1 {
		volumeType = features[1]
	}

	price, ok := o.Pricing.VolumePrices[volumeType]
	if !ok {
		price = o.Pricing.VolumePrices["high-speed"] // fallback
	}

	return &models.PV{
		Cost:  fmt.Sprintf("%f", price),
		Class: pvk.GetStorageClass(),
	}, nil
}

// ServiceAccountStatus returns the status of service account checks
func (o *OVH) ServiceAccountStatus() *models.ServiceAccountStatus {
	checks := []*models.ServiceAccountCheck{}

	// Check OVH API credentials
	if o.client != nil {
		checks = append(checks, &models.ServiceAccountCheck{
			Message: "OVH API: Connected",
			Status:  true,
		})
	} else {
		checks = append(checks, &models.ServiceAccountCheck{
			Message: "OVH API: Credentials not configured",
			Status:  false,
		})
	}

	return &models.ServiceAccountStatus{Checks: checks}
}

// ClusterManagementPricing returns cluster management pricing
// MKS has two tiers: "free" (0 EUR/h) and "standard" (0.09 EUR/h)
// This method attempts to detect the tier from the API
func (o *OVH) ClusterManagementPricing() (string, float64, error) {
	if o.client == nil {
		// No API client, assume free tier
		return "MKS Free", 0.0, nil
	}

	projectID := env.GetOVHProjectID()
	if projectID == "" {
		return "MKS Free", 0.0, nil
	}

	// Try to get usage to determine the actual tier
	var usage struct {
		HourlyUsage struct {
			ManagedKubernetesService []struct {
				Reference  string `json:"reference"`
				TotalPrice struct {
					Value float64 `json:"value"`
				} `json:"totalPrice"`
				Quantity struct {
					Value int `json:"value"`
				} `json:"quantity"`
			} `json:"managedKubernetesService"`
		} `json:"hourlyUsage"`
	}

	err := o.client.Get(fmt.Sprintf("/cloud/project/%s/usage/current", projectID), &usage)
	if err != nil {
		log.Warnf("Failed to get MKS usage, assuming free tier: %v", err)
		return "MKS Free", 0.0, nil
	}

	// Calculate hourly cost from usage data
	for _, mks := range usage.HourlyUsage.ManagedKubernetesService {
		if mks.Reference == "standard" && mks.Quantity.Value > 0 {
			// Standard tier: 0.09 EUR/hour
			hourlyRate := mks.TotalPrice.Value / float64(mks.Quantity.Value)
			return "MKS Standard", hourlyRate, nil
		}
	}

	// Default to free tier
	return "MKS Free", 0.0, nil
}

// CombinedDiscountForNode returns the combined discount for a node
func (o *OVH) CombinedDiscountForNode(instanceType string, isPreemptible bool, defaultDiscount, negotiatedDiscount float64) float64 {
	return 1.0 - ((1.0 - defaultDiscount) * (1.0 - negotiatedDiscount))
}

// Regions returns the list of available regions
func (o *OVH) Regions() []string {
	regionOverrides := env.GetRegionOverrideList()
	if len(regionOverrides) > 0 {
		return regionOverrides
	}

	regions := []string{}
	for region := range o.Pricing.Flavors {
		regions = append(regions, region)
	}
	return regions
}

// ApplyReservedInstancePricing is a no-op for OVH (no reserved instances)
func (*OVH) ApplyReservedInstancePricing(map[string]*models.Node) {}

// GetAddresses returns node addresses (not implemented)
func (*OVH) GetAddresses() ([]byte, error) {
	return nil, nil
}

// GetDisks returns disk information (not implemented)
func (*OVH) GetDisks() ([]byte, error) {
	return nil, nil
}

// GetOrphanedResources returns orphaned resources (not implemented)
func (*OVH) GetOrphanedResources() ([]models.OrphanedResource, error) {
	return nil, errors.New("not implemented")
}

// ClusterInfo returns cluster information
func (o *OVH) ClusterInfo() (map[string]string, error) {
	m := make(map[string]string)
	m["name"] = "OVH Cluster #1"

	c, err := o.GetConfig()
	if err != nil {
		return nil, err
	}
	if c.ClusterName != "" {
		m["name"] = c.ClusterName
	}

	m["provider"] = opencost.OVHProvider
	m["region"] = o.ClusterRegion
	m["account"] = o.ClusterAccountID
	m["remoteReadEnabled"] = strconv.FormatBool(env.IsRemoteEnabled())
	m["id"] = coreenv.GetClusterID()
	return m, nil
}

// UpdateConfigFromConfigMap updates config from a ConfigMap
func (o *OVH) UpdateConfigFromConfigMap(a map[string]string) (*models.CustomPricing, error) {
	return o.Config.UpdateFromMap(a)
}

// UpdateConfig updates the config
func (o *OVH) UpdateConfig(r io.Reader, updateType string) (*models.CustomPricing, error) {
	defer o.DownloadPricingData()

	return o.Config.Update(func(c *models.CustomPricing) error {
		a := make(map[string]interface{})
		err := json.NewDecoder(r).Decode(&a)
		if err != nil {
			return err
		}
		for k, v := range a {
			kUpper := utils.ToTitle.String(k)
			vstr, ok := v.(string)
			if ok {
				err := models.SetCustomPricingField(c, kUpper, vstr)
				if err != nil {
					return fmt.Errorf("error setting custom pricing field: %w", err)
				}
			} else {
				return fmt.Errorf("type error while updating config for %s", kUpper)
			}
		}

		if env.IsRemoteEnabled() {
			err := utils.UpdateClusterMeta(coreenv.GetClusterID(), c.ClusterName)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// GetConfig returns the current config
func (o *OVH) GetConfig() (*models.CustomPricing, error) {
	c, err := o.Config.GetCustomPricingData()
	if err != nil {
		return nil, err
	}
	if c.Discount == "" {
		c.Discount = "0%"
	}
	if c.NegotiatedDiscount == "" {
		c.NegotiatedDiscount = "0%"
	}
	if c.CurrencyCode == "" {
		c.CurrencyCode = "EUR"
	}
	return c, nil
}

// GetManagementPlatform returns the management platform
func (o *OVH) GetManagementPlatform() (string, error) {
	nodes := o.Clientset.GetAllNodes()
	if len(nodes) > 0 {
		if _, ok := nodes[0].Labels["k8s.ovh.net/nodepool"]; ok {
			return "MKS", nil // Managed Kubernetes Service
		}
	}
	return "", nil
}

// PricingSourceStatus returns the status of pricing sources
func (o *OVH) PricingSourceStatus() map[string]*models.PricingSource {
	return map[string]*models.PricingSource{
		OVHCatalogPricing: {
			Name:      OVHCatalogPricing,
			Enabled:   true,
			Available: o.catalogAvailable,
		},
		OVHAPIPricing: {
			Name:      OVHAPIPricing,
			Enabled:   true,
			Available: o.apiAvailable,
		},
	}
}

// Helper function to check if a slice contains a string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
