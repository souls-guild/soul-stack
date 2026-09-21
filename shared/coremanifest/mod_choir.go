package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modChoir declares core.choir.
var modChoir = schema.Module{
	Name: "choir",
	States: map[string]schema.State{
		"absent": {
			Description: "Keeper-side (on:keeper). The Voice with sid is absent from the run's Choir (ADR-044).",
			Input: schema.Input{
				"choir":       {Type: schema.String, Required: true, Description: "Name of the Choir part within the incarnation."},
				"incarnation": {Type: schema.String, Required: true, Description: "Name of the incarnation whose Choir is managed."},
				"sid":         {Type: schema.String, Required: true, Format: "sid", Source: &schema.InputSource{IncarnationHosts: true}, Description: "SID (FQDN) of the Soul whose Voice is removed from the Choir."},
			},
		},
		"present": {
			Description: "Keeper-side (on:keeper). The Voice with sid is present in the run's Choir (ADR-044).",
			Input: schema.Input{
				"choir":       {Type: schema.String, Required: true, Description: "Name of the Choir part within the incarnation."},
				"incarnation": {Type: schema.String, Required: true, Description: "Name of the incarnation whose Choir is managed."},
				"position":    {Type: schema.Int, Description: "Position of the Voice within the part (>= 0)."},
				"role":        {Type: schema.String, Description: "Free-form role label of the voice in the choir."},
				"sid":         {Type: schema.String, Required: true, Format: "sid", Source: &schema.InputSource{IncarnationHosts: true}, Description: "SID (FQDN) of the Soul whose Voice is added to the Choir."},
			},
		},
	},
}
