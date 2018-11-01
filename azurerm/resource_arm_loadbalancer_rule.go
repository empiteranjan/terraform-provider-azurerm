package azurerm

import (
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-04-01/network"
	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/helper/validation"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/azure"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/state"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/suppress"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/validate"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/utils"
)

func resourceArmLoadBalancerRule() *schema.Resource {
	return &schema.Resource{
		Create: resourceArmLoadBalancerRuleCreate,
		Read:   resourceArmLoadBalancerRuleRead,
		Update: resourceArmLoadBalancerRuleCreate,
		Delete: resourceArmLoadBalancerRuleDelete,

		Importer: &schema.ResourceImporter{
			State: loadBalancerSubResourceStateImporter,
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validateArmLoadBalancerRuleName,
			},

			"location": deprecatedLocationSchema(),

			"resource_group_name": resourceGroupNameSchema(),

			"loadbalancer_id": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: azure.ValidateResourceID,
			},

			"frontend_ip_configuration_name": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.NoZeroValues,
			},

			"frontend_ip_configuration_id": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"backend_address_pool_id": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},

			"protocol": {
				Type:             schema.TypeString,
				Required:         true,
				StateFunc:        state.AsLowerCase,
				DiffSuppressFunc: suppress.CaseDifference,
				ValidateFunc: validation.StringInSlice([]string{
					string(network.TransportProtocolAll),
					string(network.TransportProtocolTCP),
					string(network.TransportProtocolUDP),
				}, true),
			},

			"frontend_port": {
				Type:         schema.TypeInt,
				Required:     true,
				ValidateFunc: validate.PortNumberOrZero,
			},

			"backend_port": {
				Type:         schema.TypeInt,
				Required:     true,
				ValidateFunc: validate.PortNumberOrZero,
			},

			"probe_id": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},

			"enable_floating_ip": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},

			"idle_timeout_in_minutes": {
				Type:         schema.TypeInt,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.IntBetween(4, 30),
			},

			"load_distribution": {
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},
		},
	}
}

func resourceArmLoadBalancerRuleCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ArmClient).loadBalancerClient
	ctx := meta.(*ArmClient).StopContext

	loadBalancerID := d.Get("loadbalancer_id").(string)
	armMutexKV.Lock(loadBalancerID)
	defer armMutexKV.Unlock(loadBalancerID)

	loadBalancer, exists, err := retrieveLoadBalancerById(loadBalancerID, meta)
	if err != nil {
		return fmt.Errorf("Error Getting Load Balancer By ID: %+v", err)
	}
	if !exists {
		d.SetId("")
		log.Printf("[INFO] Load Balancer %q not found. Removing from state", d.Get("name").(string))
		return nil
	}

	newLbRule, err := expandAzureRmLoadBalancerRule(d, loadBalancer)
	if err != nil {
		return fmt.Errorf("Error Exanding Load Balancer Rule: %+v", err)
	}

	lbRules := append(*loadBalancer.LoadBalancerPropertiesFormat.LoadBalancingRules, *newLbRule)

	existingRule, existingRuleIndex, exists := findLoadBalancerRuleByName(loadBalancer, d.Get("name").(string))
	if exists {
		if d.Get("name").(string) == *existingRule.Name {
			// this rule is being updated/reapplied remove old copy from the slice
			lbRules = append(lbRules[:existingRuleIndex], lbRules[existingRuleIndex+1:]...)
		}
	}

	loadBalancer.LoadBalancerPropertiesFormat.LoadBalancingRules = &lbRules
	resGroup, loadBalancerName, err := resourceGroupAndLBNameFromId(d.Get("loadbalancer_id").(string))
	if err != nil {
		return fmt.Errorf("Error Getting Load Balancer Name and Group:: %+v", err)
	}

	future, err := client.CreateOrUpdate(ctx, resGroup, loadBalancerName, *loadBalancer)
	if err != nil {
		return fmt.Errorf("Error Creating/Updating LoadBalancer: %+v", err)
	}

	err = future.WaitForCompletionRef(ctx, client.Client)
	if err != nil {
		return fmt.Errorf("Error waiting for completion for Load Balancer updates: %+v", err)
	}

	read, err := client.Get(ctx, resGroup, loadBalancerName, "")
	if err != nil {
		return fmt.Errorf("Error Getting LoadBalancer: %+v", err)
	}
	if read.ID == nil {
		return fmt.Errorf("Cannot read Load Balancer %s (resource group %s) ID", loadBalancerName, resGroup)
	}

	var ruleId string
	for _, LoadBalancingRule := range *(*read.LoadBalancerPropertiesFormat).LoadBalancingRules {
		if *LoadBalancingRule.Name == d.Get("name").(string) {
			ruleId = *LoadBalancingRule.ID
		}
	}

	if ruleId == "" {
		return fmt.Errorf("Cannot find created Load Balancer Rule ID %q", ruleId)
	}

	d.SetId(ruleId)

	log.Printf("[DEBUG] Waiting for Load Balancer (%s) to become available", loadBalancerName)
	stateConf := &resource.StateChangeConf{
		Pending: []string{"Accepted", "Updating"},
		Target:  []string{"Succeeded"},
		Refresh: loadbalancerStateRefreshFunc(ctx, client, resGroup, loadBalancerName),
		Timeout: 10 * time.Minute,
	}
	if _, err := stateConf.WaitForState(); err != nil {
		return fmt.Errorf("Error waiting for Load Balancer (%s) to become available: %s", loadBalancerName, err)
	}

	return resourceArmLoadBalancerRuleRead(d, meta)
}

func resourceArmLoadBalancerRuleRead(d *schema.ResourceData, meta interface{}) error {
	id, err := parseAzureResourceID(d.Id())
	if err != nil {
		return err
	}
	name := id.Path["loadBalancingRules"]

	loadBalancer, exists, err := retrieveLoadBalancerById(d.Get("loadbalancer_id").(string), meta)
	if err != nil {
		return fmt.Errorf("Error Getting Load Balancer By ID: %+v", err)
	}
	if !exists {
		d.SetId("")
		log.Printf("[INFO] Load Balancer %q not found. Removing from state", name)
		return nil
	}

	config, _, exists := findLoadBalancerRuleByName(loadBalancer, name)
	if !exists {
		d.SetId("")
		log.Printf("[INFO] Load Balancer Rule %q not found. Removing from state", name)
		return nil
	}

	d.Set("name", config.Name)
	d.Set("resource_group_name", id.ResourceGroup)

	if properties := config.LoadBalancingRulePropertiesFormat; properties != nil {
		d.Set("protocol", properties.Protocol)
		d.Set("frontend_port", properties.FrontendPort)
		d.Set("backend_port", properties.BackendPort)

		if properties.EnableFloatingIP != nil {
			d.Set("enable_floating_ip", properties.EnableFloatingIP)
		}

		if properties.IdleTimeoutInMinutes != nil {
			d.Set("idle_timeout_in_minutes", properties.IdleTimeoutInMinutes)
		}

		if properties.FrontendIPConfiguration != nil {
			fipID, err := parseAzureResourceID(*properties.FrontendIPConfiguration.ID)
			if err != nil {
				return err
			}

			d.Set("frontend_ip_configuration_name", fipID.Path["frontendIPConfigurations"])
			d.Set("frontend_ip_configuration_id", properties.FrontendIPConfiguration.ID)
		}

		if properties.BackendAddressPool != nil {
			d.Set("backend_address_pool_id", properties.BackendAddressPool.ID)
		}

		if properties.Probe != nil {
			d.Set("probe_id", properties.Probe.ID)
		}

		if properties.LoadDistribution != "" {
			d.Set("load_distribution", properties.LoadDistribution)
		}
	}

	return nil
}

func resourceArmLoadBalancerRuleDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ArmClient).loadBalancerClient
	ctx := meta.(*ArmClient).StopContext

	loadBalancerID := d.Get("loadbalancer_id").(string)
	armMutexKV.Lock(loadBalancerID)
	defer armMutexKV.Unlock(loadBalancerID)

	loadBalancer, exists, err := retrieveLoadBalancerById(loadBalancerID, meta)
	if err != nil {
		return fmt.Errorf("Error Getting Load Balancer By ID: %+v", err)
	}
	if !exists {
		d.SetId("")
		return nil
	}

	_, index, exists := findLoadBalancerRuleByName(loadBalancer, d.Get("name").(string))
	if !exists {
		return nil
	}

	oldLbRules := *loadBalancer.LoadBalancerPropertiesFormat.LoadBalancingRules
	newLbRules := append(oldLbRules[:index], oldLbRules[index+1:]...)
	loadBalancer.LoadBalancerPropertiesFormat.LoadBalancingRules = &newLbRules

	resGroup, loadBalancerName, err := resourceGroupAndLBNameFromId(d.Get("loadbalancer_id").(string))
	if err != nil {
		return fmt.Errorf("Error Getting Load Balancer Name and Group:: %+v", err)
	}

	future, err := client.CreateOrUpdate(ctx, resGroup, loadBalancerName, *loadBalancer)
	if err != nil {
		return fmt.Errorf("Error Creating/Updating Load Balancer %q (Resource Group %q): %+v", loadBalancerName, resGroup, err)
	}

	err = future.WaitForCompletionRef(ctx, client.Client)
	if err != nil {
		return fmt.Errorf("Error waiting for completion of Load Balancer %q (Resource Group %q): %+v", loadBalancerName, resGroup, err)
	}

	read, err := client.Get(ctx, resGroup, loadBalancerName, "")
	if err != nil {
		return fmt.Errorf("Error Getting LoadBalancer: %+v", err)
	}
	if read.ID == nil {
		return fmt.Errorf("Cannot read ID of Load Balancer %q (resource group %s)", loadBalancerName, resGroup)
	}

	return nil
}

func expandAzureRmLoadBalancerRule(d *schema.ResourceData, lb *network.LoadBalancer) (*network.LoadBalancingRule, error) {

	properties := network.LoadBalancingRulePropertiesFormat{
		Protocol:         network.TransportProtocol(d.Get("protocol").(string)),
		FrontendPort:     utils.Int32(int32(d.Get("frontend_port").(int))),
		BackendPort:      utils.Int32(int32(d.Get("backend_port").(int))),
		EnableFloatingIP: utils.Bool(d.Get("enable_floating_ip").(bool)),
	}

	if v, ok := d.GetOk("idle_timeout_in_minutes"); ok {
		properties.IdleTimeoutInMinutes = utils.Int32(int32(v.(int)))
	}

	if v := d.Get("load_distribution").(string); v != "" {
		properties.LoadDistribution = network.LoadDistribution(v)
	}

	if v := d.Get("frontend_ip_configuration_name").(string); v != "" {
		rule, exists := findLoadBalancerFrontEndIpConfigurationByName(lb, v)
		if !exists {
			return nil, fmt.Errorf("[ERROR] Cannot find FrontEnd IP Configuration with the name %s", v)
		}

		properties.FrontendIPConfiguration = &network.SubResource{
			ID: rule.ID,
		}
	}

	if v := d.Get("backend_address_pool_id").(string); v != "" {
		properties.BackendAddressPool = &network.SubResource{
			ID: &v,
		}
	}

	if v := d.Get("probe_id").(string); v != "" {
		properties.Probe = &network.SubResource{
			ID: &v,
		}
	}

	return &network.LoadBalancingRule{
		Name: utils.String(d.Get("name").(string)),
		LoadBalancingRulePropertiesFormat: &properties,
	}, nil
}

func validateArmLoadBalancerRuleName(v interface{}, k string) (ws []string, errors []error) {
	value := v.(string)
	if !regexp.MustCompile(`^[a-zA-Z_0-9.-]+$`).MatchString(value) {
		errors = append(errors, fmt.Errorf(
			"only word characters, numbers, underscores, periods, and hyphens allowed in %q: %q",
			k, value))
	}

	if len(value) > 80 {
		errors = append(errors, fmt.Errorf(
			"%q cannot be longer than 80 characters: %q", k, value))
	}

	if len(value) == 0 {
		errors = append(errors, fmt.Errorf(
			"%q cannot be an empty string: %q", k, value))
	}
	if !regexp.MustCompile(`[a-zA-Z0-9_]$`).MatchString(value) {
		errors = append(errors, fmt.Errorf(
			"%q must end with a word character, number, or underscore: %q", k, value))
	}

	if !regexp.MustCompile(`^[a-zA-Z0-9]`).MatchString(value) {
		errors = append(errors, fmt.Errorf(
			"%q must start with a word character or number: %q", k, value))
	}

	return ws, errors
}
