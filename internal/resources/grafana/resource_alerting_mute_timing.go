package grafana

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/grafana/grafana-openapi-client-go/client/provisioning"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/terraform-provider-grafana/internal/common"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func ResourceMuteTiming() *schema.Resource {
	return &schema.Resource{
		Description: `
Manages Grafana Alerting mute timings.

* [Official documentation](https://grafana.com/docs/grafana/latest/alerting/manage-notifications/mute-timings/)
* [HTTP API](https://grafana.com/docs/grafana/next/developers/http_api/alerting_provisioning/#mute-timings)

This resource requires Grafana 9.1.0 or later.
`,

		CreateContext: common.WithAlertingMutex[schema.CreateContextFunc](createMuteTiming),
		ReadContext:   readMuteTiming,
		UpdateContext: common.WithAlertingMutex[schema.UpdateContextFunc](updateMuteTiming),
		DeleteContext: common.WithAlertingMutex[schema.DeleteContextFunc](deleteMuteTiming),
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		SchemaVersion: 0,
		Schema: map[string]*schema.Schema{
			"org_id": orgIDAttribute(),
			"name": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The name of the mute timing.",
			},

			"intervals": {
				// List instead of set is necessary here. We rely on diff-suppression on the `months` field.
				// TF represents sets internally as dics, with hashes as keys.
				// If we use a set, the object hash is different any time a nested object gets changed.
				// Therefore TF will see delete+create instead of modify, which breaks the diff-suppression.
				Type:        schema.TypeList,
				Optional:    true,
				Description: "The time intervals at which to mute notifications.",
				Elem: &schema.Resource{
					SchemaVersion: 0,
					Schema: map[string]*schema.Schema{
						"times": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: "The time ranges, represented in minutes, during which to mute in a given day.",
							Elem: &schema.Resource{
								SchemaVersion: 0,
								Schema: map[string]*schema.Schema{
									"start": {
										Type:        schema.TypeString,
										Required:    true,
										Description: "The time, in hh:mm format, of when the interval should begin inclusively.",
									},
									"end": {
										Type:        schema.TypeString,
										Required:    true,
										Description: "The time, in hh:mm format, of when the interval should end exclusively.",
									},
								},
							},
						},
						"weekdays": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `An inclusive range of weekdays, e.g. "monday" or "tuesday:thursday".`,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"days_of_month": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `An inclusive range of days, 1-31, within a month, e.g. "1" or "14:16". Negative values can be used to represent days counting from the end of a month, e.g. "-1".`,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"months": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `An inclusive range of months, either numerical or full calendar month, e.g. "1:3", "december", or "may:august".`,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
							DiffSuppressFunc: suppressMonthDiff,
						},
						"years": {
							Type:        schema.TypeList,
							Optional:    true,
							Description: `A positive inclusive range of years, e.g. "2030" or "2025:2026".`,
							Elem: &schema.Schema{
								Type: schema.TypeString,
							},
						},
						"location": {
							Type:        schema.TypeString,
							Optional:    true,
							Description: `Provides the time zone for the time interval. Must be a location in the IANA time zone database, e.g "America/New_York"`,
						},
					},
				},
			},
		},
	}
}

func readMuteTiming(ctx context.Context, data *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client, orgID, name := OAPIClientFromExistingOrgResource(meta, data.Id())

	resp, err := client.Provisioning.GetMuteTiming(name)
	if err, shouldReturn := common.CheckReadError("mute timing", data, err); shouldReturn {
		return err
	}
	mt := resp.Payload

	data.SetId(MakeOrgResourceID(orgID, mt.Name))
	data.Set("org_id", strconv.FormatInt(orgID, 10))
	data.Set("name", mt.Name)
	data.Set("intervals", packIntervals(mt.TimeIntervals))
	return nil
}

func createMuteTiming(ctx context.Context, data *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client, orgID := OAPIClientFromNewOrgResource(meta, data)

	intervals := data.Get("intervals").([]interface{})
	params := provisioning.NewPostMuteTimingParams().
		WithBody(&models.MuteTimeInterval{
			Name:          data.Get("name").(string),
			TimeIntervals: unpackIntervals(intervals),
		})

	resp, err := client.Provisioning.PostMuteTiming(params)
	if err != nil {
		return diag.FromErr(err)
	}
	data.SetId(MakeOrgResourceID(orgID, resp.Payload.Name))
	return readMuteTiming(ctx, data, meta)
}

func updateMuteTiming(ctx context.Context, data *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client, _, name := OAPIClientFromExistingOrgResource(meta, data.Id())

	intervals := data.Get("intervals").([]interface{})
	params := provisioning.NewPutMuteTimingParams().
		WithName(name).
		WithBody(&models.MuteTimeInterval{
			Name:          name,
			TimeIntervals: unpackIntervals(intervals),
		})

	_, err := client.Provisioning.PutMuteTiming(params)
	if err != nil {
		return diag.FromErr(err)
	}
	return readMuteTiming(ctx, data, meta)
}

func deleteMuteTiming(ctx context.Context, data *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client, _, name := OAPIClientFromExistingOrgResource(meta, data.Id())

	_, err := client.Provisioning.DeleteMuteTiming(name)
	diag, _ := common.CheckReadError("mute timing", data, err)
	return diag
}

func suppressMonthDiff(k, oldValue, newValue string, d *schema.ResourceData) bool {
	monthNums := map[string]int{
		"january":   1,
		"february":  2,
		"march":     3,
		"april":     4,
		"may":       5,
		"june":      6,
		"july":      7,
		"august":    8,
		"september": 9,
		"october":   10,
		"november":  11,
		"december":  12,
	}

	oldNormalized := oldValue
	newNormalized := newValue
	for k, v := range monthNums {
		oldNormalized = strings.ReplaceAll(oldNormalized, k, fmt.Sprint(v))
		newNormalized = strings.ReplaceAll(newNormalized, k, fmt.Sprint(v))
	}

	return oldNormalized == newNormalized
}

func packIntervals(nts []*models.TimeInterval) []interface{} {
	if nts == nil {
		return nil
	}

	intervals := make([]interface{}, 0, len(nts))
	for _, ti := range nts {
		in := map[string]interface{}{}
		if ti.Times != nil {
			times := make([]interface{}, 0, len(ti.Times))
			for _, time := range ti.Times {
				times = append(times, packTimeRange(time))
			}
			in["times"] = times
		}
		if ti.Weekdays != nil {
			in["weekdays"] = common.StringSliceToList(ti.Weekdays)
		}
		if ti.DaysOfMonth != nil {
			in["days_of_month"] = common.StringSliceToList(ti.DaysOfMonth)
		}
		if ti.Months != nil {
			in["months"] = common.StringSliceToList(ti.Months)
		}
		if ti.Years != nil {
			in["years"] = common.StringSliceToList(ti.Years)
		}
		if ti.Location != "" {
			in["location"] = ti.Location
		}
		intervals = append(intervals, in)
	}

	return intervals
}

func unpackIntervals(raw []interface{}) []*models.TimeInterval {
	if raw == nil {
		return nil
	}

	result := make([]*models.TimeInterval, len(raw))
	for i, r := range raw {
		interval := models.TimeInterval{}
		block := r.(map[string]interface{})

		if vals, ok := block["times"]; ok && vals != nil {
			vals := vals.([]interface{})
			interval.Times = make([]*models.TimeIntervalRange, len(vals))
			for i := range vals {
				interval.Times[i] = unpackTimeRange(vals[i])
			}
		}
		if vals, ok := block["weekdays"]; ok && vals != nil {
			interval.Weekdays = common.ListToStringSlice(vals.([]interface{}))
		}
		if vals, ok := block["days_of_month"]; ok && vals != nil {
			interval.DaysOfMonth = common.ListToStringSlice(vals.([]interface{}))
		}
		if vals, ok := block["months"]; ok && vals != nil {
			interval.Months = common.ListToStringSlice(vals.([]interface{}))
		}
		if vals, ok := block["years"]; ok && vals != nil {
			interval.Years = common.ListToStringSlice(vals.([]interface{}))
		}

		if vals, ok := block["location"]; ok && vals != nil {
			interval.Location = vals.(string)
		}

		result[i] = &interval
	}

	return result
}

func packTimeRange(time *models.TimeIntervalRange) interface{} {
	return map[string]string{
		"start": time.StartTime,
		"end":   time.EndTime,
	}
}

func unpackTimeRange(raw interface{}) *models.TimeIntervalRange {
	vals := raw.(map[string]interface{})
	return &models.TimeIntervalRange{
		StartTime: vals["start"].(string),
		EndTime:   vals["end"].(string),
	}
}
