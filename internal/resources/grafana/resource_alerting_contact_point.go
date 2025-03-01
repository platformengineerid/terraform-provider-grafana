package grafana

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafana-openapi-client-go/client/provisioning"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

	"github.com/grafana/terraform-provider-grafana/internal/common"
)

var notifiers = []notifier{
	alertmanagerNotifier{},
	dingDingNotifier{},
	discordNotifier{},
	emailNotifier{},
	googleChatNotifier{},
	kafkaNotifier{},
	lineNotifier{},
	oncallNotifier{},
	opsGenieNotifier{},
	pagerDutyNotifier{},
	pushoverNotifier{},
	sensugoNotifier{},
	slackNotifier{},
	teamsNotifier{},
	telegramNotifier{},
	threemaNotifier{},
	victorOpsNotifier{},
	webexNotifier{},
	webhookNotifier{},
	wecomNotifier{},
}

func ResourceContactPoint() *schema.Resource {
	resource := &schema.Resource{
		Description: `
Manages Grafana Alerting contact points.

* [Official documentation](https://grafana.com/docs/grafana/next/alerting/fundamentals/contact-points/)
* [HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/alerting_provisioning/#contact-points)

This resource requires Grafana 9.1.0 or later.
`,
		CreateContext: common.WithAlertingMutex[schema.CreateContextFunc](updateContactPoint),
		ReadContext:   readContactPoint,
		UpdateContext: common.WithAlertingMutex[schema.UpdateContextFunc](updateContactPoint),
		DeleteContext: common.WithAlertingMutex[schema.DeleteContextFunc](deleteContactPoint),

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		SchemaVersion: 0,
		Schema: map[string]*schema.Schema{
			"org_id": orgIDAttribute(),
			"name": {
				Type:        schema.TypeString,
				ForceNew:    true,
				Required:    true,
				Description: "The name of the contact point.",
			},
		},
	}

	// Build list of available notifier fields, at least one has to be specified
	notifierFields := make([]string, len(notifiers))
	for i, n := range notifiers {
		notifierFields[i] = n.meta().field
	}

	for _, n := range notifiers {
		resource.Schema[n.meta().field] = &schema.Schema{
			Type:         schema.TypeSet,
			Optional:     true,
			Description:  n.meta().desc,
			Elem:         n.schema(),
			AtLeastOneOf: notifierFields,
		}
	}

	return resource
}

func readContactPoint(ctx context.Context, data *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client, orgID, name := OAPIClientFromExistingOrgResource(meta, data.Id())

	// First, try to fetch the contact point by name.
	// If that fails, try to fetch it by the UID of its notifiers.
	resp, err := client.Provisioning.GetContactpoints(provisioning.NewGetContactpointsParams().WithName(&name))
	if err != nil {
		return diag.FromErr(err)
	}
	points := resp.Payload
	if len(points) == 0 {
		// If the contact point was not found by name, try to fetch it by UID.
		// This is a deprecated ID format (uid;uid2;uid3)
		// TODO: Remove on the next major version
		uidsMap := map[string]bool{}
		for _, uid := range strings.Split(data.Id(), ";") {
			uidsMap[uid] = false
		}
		resp, err := client.Provisioning.GetContactpoints(provisioning.NewGetContactpointsParams())
		if err != nil {
			return diag.FromErr(err)
		}
		for i, p := range resp.Payload {
			if _, ok := uidsMap[p.UID]; !ok {
				continue
			}
			uidsMap[p.UID] = true
			points = append(points, p)
			if i > 0 && p.Name != points[0].Name {
				return diag.FromErr(fmt.Errorf("contact point with UID %s has a different name (%s) than the contact point with UID %s (%s)", p.UID, p.Name, points[0].UID, points[0].Name))
			}
		}

		for uid, found := range uidsMap {
			if !found {
				// Since this is an import, all UIDs should exist
				return diag.FromErr(fmt.Errorf("contact point with UID %s was not found", uid))
			}
		}
	}

	if len(points) == 0 {
		return common.WarnMissing("contact point", data)
	}

	if err := packContactPoints(points, data); err != nil {
		return diag.FromErr(err)
	}
	data.Set("org_id", strconv.FormatInt(orgID, 10))
	data.SetId(MakeOrgResourceID(orgID, points[0].Name))

	return nil
}

func updateContactPoint(ctx context.Context, data *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client, orgID := OAPIClientFromNewOrgResource(meta, data)

	ps := unpackContactPoints(data)

	// If the contact point already exists, we need to fetch its current state so that we can compare it to the proposed state.
	var currentPoints models.ContactPoints
	if !data.IsNewResource() {
		name := data.Get("name").(string)
		resp, err := client.Provisioning.GetContactpoints(provisioning.NewGetContactpointsParams().WithName(&name))
		if err != nil && !common.IsNotFoundError(err) {
			return diag.FromErr(err)
		}
		if resp != nil {
			currentPoints = resp.Payload
		}
	}

	processedUIDs := map[string]bool{}
	for i := range ps {
		p := ps[i]
		var uid string
		if uid = p.tfState["uid"].(string); uid != "" {
			// If the contact point already has a UID, update it.
			params := provisioning.NewPutContactpointParams().WithUID(uid).WithBody(p.gfState)
			if _, err := client.Provisioning.PutContactpoint(params); err != nil {
				return diag.FromErr(err)
			}
		} else {
			// If the contact point does not have a UID, create it.
			// Retry if the API returns 500 because it may be that the alertmanager is not ready in the org yet.
			// The alertmanager is provisioned asynchronously when the org is created.
			err := retry.RetryContext(ctx, 2*time.Minute, func() *retry.RetryError {
				resp, err := client.Provisioning.PostContactpoints(provisioning.NewPostContactpointsParams().WithBody(p.gfState))
				if orgID > 1 && err != nil && err.(*runtime.APIError).IsCode(500) {
					return retry.RetryableError(err)
				} else if err != nil {
					return retry.NonRetryableError(err)
				}
				uid = resp.Payload.UID
				return nil
			})
			if err != nil {
				return diag.FromErr(err)
			}
		}

		// Since this is a new resource, the proposed state won't have a UID.
		// We need the UID so that we can later associate it with the config returned in the api response.
		ps[i].tfState["uid"] = uid
		processedUIDs[uid] = true
	}

	for _, p := range currentPoints {
		if _, ok := processedUIDs[p.UID]; !ok {
			// If the contact point is not in the proposed state, delete it.
			if _, err := client.Provisioning.DeleteContactpoints(p.UID); err != nil {
				return diag.Errorf("failed to remove contact point notifier with UID %s from contact point %s: %v", p.UID, p.Name, err)
			}
		}
	}

	data.SetId(MakeOrgResourceID(orgID, data.Get("name").(string)))
	return readContactPoint(ctx, data, meta)
}

func deleteContactPoint(ctx context.Context, data *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client, _, name := OAPIClientFromExistingOrgResource(meta, data.Id())

	resp, err := client.Provisioning.GetContactpoints(provisioning.NewGetContactpointsParams().WithName(&name))
	if err, shouldReturn := common.CheckReadError("contact point", data, err); shouldReturn {
		return err
	}

	for _, cp := range resp.Payload {
		if _, err := client.Provisioning.DeleteContactpoints(cp.UID); err != nil {
			return diag.FromErr(err)
		}
	}

	return nil
}

func unpackContactPoints(data *schema.ResourceData) []statePair {
	result := make([]statePair, 0)
	name := data.Get("name").(string)
	for _, n := range notifiers {
		if points, ok := data.GetOk(n.meta().field); ok {
			for _, p := range points.(*schema.Set).List() {
				result = append(result, statePair{
					tfState: p.(map[string]interface{}),
					gfState: unpackPointConfig(n, p, name),
				})
			}
		}
	}

	return result
}

func unpackPointConfig(n notifier, data interface{}, name string) *models.EmbeddedContactPoint {
	pt := n.unpack(data, name)
	settings := pt.Settings.(map[string]interface{})
	// Treat settings like `omitempty`. Workaround for versions affected by https://github.com/grafana/grafana/issues/55139
	for k, v := range settings {
		if v == "" {
			delete(settings, k)
		}
	}
	return pt
}

func packContactPoints(ps []*models.EmbeddedContactPoint, data *schema.ResourceData) error {
	pointsPerNotifier := map[notifier][]interface{}{}
	for _, p := range ps {
		data.Set("name", p.Name)

		for _, n := range notifiers {
			if *p.Type == n.meta().typeStr {
				packed, err := n.pack(p, data)
				if err != nil {
					return err
				}
				pointsPerNotifier[n] = append(pointsPerNotifier[n], packed)
				continue
			}
		}
	}

	for n, pts := range pointsPerNotifier {
		data.Set(n.meta().field, pts)
	}

	return nil
}

func unpackCommonNotifierFields(raw map[string]interface{}) (string, bool, map[string]interface{}) {
	return raw["uid"].(string), raw["disable_resolve_message"].(bool), raw["settings"].(map[string]interface{})
}

func packCommonNotifierFields(p *models.EmbeddedContactPoint) map[string]interface{} {
	return map[string]interface{}{
		"uid":                     p.UID,
		"disable_resolve_message": p.DisableResolveMessage,
	}
}

func packSettings(p *models.EmbeddedContactPoint) map[string]interface{} {
	settings := map[string]interface{}{}
	for k, v := range p.Settings.(map[string]interface{}) {
		settings[k] = fmt.Sprintf("%#v", v)
	}
	return settings
}

func commonNotifierResource() *schema.Resource {
	return &schema.Resource{
		Schema: map[string]*schema.Schema{
			"uid": {
				Type:        schema.TypeString,
				Computed:    true,
				Description: "The UID of the contact point.",
			},
			"disable_resolve_message": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
				Description: "Whether to disable sending resolve messages.",
			},
			"settings": {
				Type:        schema.TypeMap,
				Optional:    true,
				Sensitive:   true,
				Default:     map[string]interface{}{},
				Description: "Additional custom properties to attach to the notifier.",
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},
		},
	}
}

type notifier interface {
	meta() notifierMeta
	schema() *schema.Resource
	pack(p *models.EmbeddedContactPoint, data *schema.ResourceData) (interface{}, error)
	unpack(raw interface{}, name string) *models.EmbeddedContactPoint
}

type notifierMeta struct {
	field        string
	typeStr      string
	desc         string
	secureFields []string
}

type statePair struct {
	tfState map[string]interface{}
	gfState *models.EmbeddedContactPoint
}

func packNotifierStringField(gfSettings, tfSettings *map[string]interface{}, gfKey, tfKey string) {
	if v, ok := (*gfSettings)[gfKey]; ok && v != nil {
		(*tfSettings)[tfKey] = v.(string)
		delete(*gfSettings, gfKey)
	}
}

func packSecureFields(tfSettings, state map[string]interface{}, secureFields []string) {
	for _, tfKey := range secureFields {
		if v, ok := state[tfKey]; ok && v != nil {
			tfSettings[tfKey] = v.(string)
		}
	}
}

func unpackNotifierStringField(tfSettings, gfSettings *map[string]interface{}, tfKey, gfKey string) {
	if v, ok := (*tfSettings)[tfKey]; ok && v != nil {
		(*gfSettings)[gfKey] = v.(string)
	}
}

func getNotifierConfigFromStateWithUID(data *schema.ResourceData, n notifier, uid string) map[string]interface{} {
	if points, ok := data.GetOk(n.meta().field); ok {
		for _, pt := range points.(*schema.Set).List() {
			config := pt.(map[string]interface{})
			if config["uid"] == uid {
				return config
			}
		}
	}

	return nil
}
