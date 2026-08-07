package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modArchive declares core.archive.
var modArchive = schema.Module{
	Name:         "archive",
	Capabilities: []schema.Capability{schema.FSWriteRoot},
	States: map[string]schema.State{
		"extracted": {
			Description: "Extract the archive at path into the dest directory (tar/tar.gz/tar.bz2/zip); ADR-015.",
			Input: schema.Input{
				"dest":        {Type: schema.String, Required: true, Description: "Extraction directory."},
				"format":      {Type: schema.String, Description: "Format (tar|tar.gz|tar.bz2|zip); omitted — auto-detect by extension."},
				"max_entries": {Type: schema.Integer, Description: "Limit on the number of entries in the archive; default 100000. Zip-bomb protection."},
				"max_ratio":   {Type: schema.Integer, Description: "Limit on the ratio of extracted to compressed bytes (compression ratio); default 100, 0 — disabled. Zip-bomb protection for a small compressed size."},
				"max_size":    {Type: schema.String, Description: "Limit on total extracted size (number of bytes or N[KiB|MiB|GiB]); default 1GiB. Zip-bomb protection."},
				"path":        {Type: schema.String, Required: true, Description: "Path to the source archive."},
			},
		},
	},
}
