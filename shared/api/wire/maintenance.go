package wire

import "time"

// MaintenanceWindowRequest — the body of POST /v1/incarnations/{id}/maintenance-windows
// and of PUT .../maintenance-windows/{window_id}, which replaces every field (NIM-1072).
// A window is `weekly` (days, start_time, duration_minutes, timezone) or `once`
// (starts_at, ends_at, NIM-1137); a field of the other kind is refused with 422.
type MaintenanceWindowRequest struct {
	Kind            string    `json:"kind,omitempty" enum:"weekly,once" doc:"weekly (the default when omitted) or once; each kind takes only its own fields, and a field of the other kind is refused with 422 naming it"`
	Days            []string  `json:"days,omitempty" minItems:"1" maxItems:"7" uniqueItems:"true" enum:"mon,tue,wed,thu,fri,sat,sun" doc:"weekly, required: weekdays a slot starts on; a slot belongs to the day it starts on, even when it crosses midnight"`
	StartTime       string    `json:"start_time,omitempty" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"weekly, required: wall-clock start of each slot, HH:MM, in timezone. A time the spring DST change skips moves forward by the gap; a time the autumn change repeats is its first occurrence"`
	DurationMinutes int       `json:"duration_minutes,omitempty" minimum:"1" maximum:"1440" doc:"weekly, required: slot length in minutes of elapsed time, at most a day; a slot over a DST change still lasts exactly this long"`
	Timezone        string    `json:"timezone,omitempty" minLength:"1" maxLength:"64" doc:"weekly, required: IANA zone of start_time, e.g. Europe/Moscow or UTC; names that depend on the Keeper host (Local, localtime, posix/...) are refused"`
	StartsAt        time.Time `json:"starts_at,omitempty" doc:"once, required: start of the window, RFC 3339 (inclusive)"`
	EndsAt          time.Time `json:"ends_at,omitempty" doc:"once, required: end of the window, RFC 3339 (exclusive); after starts_at. Once it has passed the window no longer counts"`
	Comment         string    `json:"comment,omitempty" maxLength:"500" doc:"free text, at most 500 characters"`
}

// MaintenanceWindow — one maintenance window of an incarnation, with the fields
// of its kind only.
type MaintenanceWindow struct {
	ID              string     `json:"id" doc:"window id (UUID)"`
	Kind            string     `json:"kind" enum:"weekly,once" doc:"weekly or once"`
	Days            []string   `json:"days,omitempty" doc:"weekly: weekdays a slot starts on, in calendar order"`
	StartTime       string     `json:"start_time,omitempty" doc:"weekly: HH:MM in timezone"`
	DurationMinutes int        `json:"duration_minutes,omitempty" doc:"weekly: slot length in minutes"`
	Timezone        string     `json:"timezone,omitempty" doc:"weekly: IANA zone of start_time"`
	StartsAt        *time.Time `json:"starts_at,omitempty" doc:"once: start, inclusive"`
	EndsAt          *time.Time `json:"ends_at,omitempty" doc:"once: end, exclusive. A once window whose end has passed is listed until the Reaper deletes it, and no longer counts"`
	Comment         string     `json:"comment"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// MaintenanceStatus — what the window calculator says at the moment of the read.
type MaintenanceStatus struct {
	OpenNow      bool      `json:"open_now" doc:"a run may start now: inside some window (or there are no windows, a once window that has ended being none) and outside every freeze"`
	NextStart    time.Time `json:"next_start" doc:"the first instant at or after now at which a run may start; now itself when open_now"`
	ActiveFreeze *Freeze   `json:"active_freeze,omitempty" doc:"the freeze in force now - the one that ends last when several overlap; absent when none"`
}

// MaintenanceWindowListReply — the 200 body of GET /v1/incarnations/{id}/maintenance-windows:
// every window, in creation order, once windows that have ended included until the
// Reaper deletes them, and the status over windows and freezes together.
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
