package parity

// Background jobs. The web UI follows a job's log through jobEvents() in
// $lib/api (the JobLog component).
func init() {
	register(
		op("jobs-list", cli("job list"), web("routes/jobs/")),
		op("jobs-get", cli("job show", "job wait"), web("routes/jobs/")),
		op("jobs-events", cli("job wait"), web("lib/api.ts")),
	)
}
