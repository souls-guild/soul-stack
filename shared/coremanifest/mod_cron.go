package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modCron declares core.cron.
var modCron = schema.Module{
	Name:         "cron",
	Capabilities: []schema.Capability{schema.RunAsRoot, schema.FSWriteRoot},
	States: map[string]schema.State{
		"absent": {
			Description: "Job file removed.",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "Job name (= file name in /etc/cron.d)."},
			},
		},
		"present": {
			Description: "Job file /etc/cron.d/<name> with the given schedule and command (ADR-015).",
			Input: schema.Input{
				"command":  {Type: schema.String, Required: true, Description: "Command run on schedule."},
				"name":     {Type: schema.String, Required: true, Description: "Job name (= file name in /etc/cron.d, [A-Za-z0-9_-])."},
				"schedule": {Type: schema.String, Required: true, Description: "Cron schedule (5 fields), e.g. \"0 3 * * *\"."},
				"user":     {Type: schema.String, Description: "User to run as (default root)."},
			},
		},
	},
}
