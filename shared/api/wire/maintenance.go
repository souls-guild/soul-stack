package wire

import "time"

// MaintenanceWindowRequest — the body of POST /v1/incarnations/{id}/maintenance-windows
// and of PUT .../maintenance-windows/{window_id}, which replaces every field (NIM-1072).
type MaintenanceWindowRequest struct {
	Days            []string `json:"days" required:"true" minItems:"1" maxItems:"7" uniqueItems:"true" enum:"mon,tue,wed,thu,fri,sat,sun" doc:"weekdays a slot starts on; a slot belongs to the day it starts on, even when it crosses midnight"`
	StartTime       string   `json:"start_time" required:"true" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"wall-clock start of each slot, HH:MM, in timezone. A time the spring DST change skips moves forward by the gap; a time the autumn change repeats is its first occurrence"`
	DurationMinutes int      `json:"duration_minutes" required:"true" minimum:"1" maximum:"1440" doc:"slot length in minutes of elapsed time, at most a day; a slot over a DST change still lasts exactly this long"`
	Timezone        string   `json:"timezone" required:"true" minLength:"1" maxLength:"64" doc:"IANA zone of start_time, e.g. Europe/Moscow or UTC; names that depend on the Keeper host (Local, localtime, posix/...) are refused"`
	Comment         string   `json:"comment,omitempty" maxLength:"500" doc:"free text, at most 500 characters"`
}

// MaintenanceWindow — one maintenance window of an incarnation.
type MaintenanceWindow struct {
	ID              string    `json:"id" doc:"window id (UUID)"`
	Days            []string  `json:"days" doc:"weekdays a slot starts on, in calendar order"`
	StartTime       string    `json:"start_time" doc:"HH:MM in timezone"`
	DurationMinutes int       `json:"duration_minutes"`
	Timezone        string    `json:"timezone"`
	Comment         string    `json:"comment"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MaintenanceStatus — what the window calculator says at the moment of the read.
type MaintenanceStatus struct {
	OpenNow      bool      `json:"open_now" doc:"a run may start now: inside a slot of some window (or there are no windows) and outside every freeze"`
	NextStart    time.Time `json:"next_start" doc:"the first instant at or after now at which a run may start; now itself when open_now"`
	ActiveFreeze *Freeze   `json:"active_freeze,omitempty" doc:"the freeze in force now - the one that ends last when several overlap; absent when none"`
}

// MaintenanceWindowListReply — the 200 body of GET /v1/incarnations/{id}/maintenance-windows:
// every window, in creation order, and the status over windows and freezes together.
type MaintenanceWindowListReply struct {
	Items  []MaintenanceWindow `json:"items"`
	Status MaintenanceStatus   `json:"status"`
}

// FreezeRequest — the body of POST /v1/incarnations/{id}/freezes and of
// PUT .../freezes/{freeze_id}, which replaces every field.
type FreezeRequest struct {
	StartsAt time.Time `json:"starts_at" required:"true" doc:"start of the freeze, RFC 3339 (inclusive)"`
	EndsAt   time.Time `json:"ends_at" required:"true" doc:"end of the freeze, RFC 3339 (exclusive); after starts_at"`
	Comment  string    `json:"comment,omitempty" maxLength:"500" doc:"free text, at most 500 characters"`
}

// Freeze — one freeze of an incarnation: [starts_at, ends_at) during which no
// automatic run starts.
type Freeze struct {
	ID        string    `json:"id" doc:"freeze id (UUID)"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FreezeListReply — the 200 body of GET /v1/incarnations/{id}/freezes, past freezes
// included, ordered by start.
type FreezeListReply struct {
	Items []Freeze `json:"items"`
}
