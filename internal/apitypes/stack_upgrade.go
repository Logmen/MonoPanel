package apitypes

import "time"

// StackUpgrade is a component's update waiting in its repositories: what
// mp stack upgrade installs.
type StackUpgrade struct {
	// Component is how mp stack upgrade names it: nginx, php, apache, db.
	Component string `json:"component"`
	// From and To are the versions of the component's main package (nginx,
	// php<v>-fpm, apache2, the server), or of the first package with an
	// update when the main one has none; Origin is where To comes from (an
	// apt repository's host, a dnf repository's id).
	From   string `json:"from"`
	To     string `json:"to"`
	Origin string `json:"origin,omitempty"`
	// Packages are all of the component's packages with an update.
	Packages []PackageUpgrade `json:"packages"`
}

// PackageUpgrade is one package's update.
type PackageUpgrade struct {
	Name   string `json:"name"`
	From   string `json:"from"`
	To     string `json:"to"`
	Origin string `json:"origin,omitempty"`
}

// StackUpgradeRequest upgrades a component's packages from their
// repositories; all is nginx, PHP and Apache — the database server only by
// name, its restart is downtime.
type StackUpgradeRequest struct {
	Component string `json:"component,omitempty" enum:"nginx,php,apache,db,all" doc:"What to upgrade; all (the default) is nginx, PHP and Apache, the database server is upgraded only as db"`
}

// StackAutoUpgrade is the daily upgrade of nginx, PHP and Apache.
type StackAutoUpgrade struct {
	Enabled bool `json:"enabled"`
	// Hour is when it runs, in the server's local time.
	Hour int `json:"hour"`
	// Timezone is the server's zone the hour is in.
	Timezone string `json:"timezone"`
	// NextRun is when the next run is due while it is enabled.
	NextRun *time.Time `json:"next_run,omitempty"`
	// Last is the outcome of the last upgrade, daily or started by hand.
	Last *StackUpgradeRun `json:"last,omitempty"`
}

// StackUpgradeRun is what an upgrade did.
type StackUpgradeRun struct {
	JobID     int64     `json:"job_id"`
	Component string    `json:"component"`
	Scheduled bool      `json:"scheduled"`
	At        time.Time `json:"at"`
	// Upgraded lists the packages it upgraded, "name from → to".
	Upgraded []string `json:"upgraded,omitempty"`
	// Error is why it failed: a package manager's error, or a validation
	// (nginx -t, php-fpm -t, apache2 -t) that kept a service from reloading.
	Error string `json:"error,omitempty"`
}

// StackAutoUpgradeUpdate changes the daily upgrade; a field left out stays.
type StackAutoUpgradeUpdate struct {
	Enabled *bool `json:"enabled,omitempty" doc:"Run the daily upgrade of nginx, PHP and Apache"`
	Hour    *int  `json:"hour,omitempty" minimum:"0" maximum:"23" doc:"Hour of the day it runs, server time"`
}
