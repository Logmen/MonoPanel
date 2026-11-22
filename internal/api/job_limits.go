package api

import (
	"time"

	"monopanel/internal/agent"
)

// What a backup gives restic: the agent lets restic back up or restore for
// resticLimit (ToolRequest.TimeoutSeconds), and the retention's forget
// --prune for pruneLimit.
const (
	resticLimit = 6 * time.Hour
	pruneLimit  = time.Hour
)

// jobLimits are the job types whose work may outlast the runner's default
// hour (jobs.DefaultLimit), with their own limits (jobs.Runner.SetLimit, at
// New):
//   - backup.run: restic's backup and the retention's prune, each with its
//     own limit, and an hour before them, which usually covers the package
//     install, the repository's check and the database dumps — a budget,
//     not a promise: each dump may take up to an hour itself (dumpDatabase),
//     and a server with several big databases leaves restic less of its own
//     limit before the job's ends;
//   - backup.restore: restic's restore, the move into place included, and an
//     hour more;
//   - migrate.run: a whole account (user:<login>) — its home with the files
//     of every site, then each of its databases, then the Maildirs of each
//     mail domain, every one of them a stream the agent ends at
//     agent.StreamLimit, one after another. A day is four of them: the
//     home's whole limit and three times as much for the databases and the
//     mail, and still an end for a migration whose source stopped answering.
//
// Every other type keeps the default hour: what it waits for takes minutes,
// and a package run of the stack's, PHP's or the mail's jobs, which the
// agent bounds itself (two hours), goes on to its end in its own unit
// should the job's hour end first. While these long jobs hold every worker
// the runner's spare one takes the other jobs (jobs.Runner.Start), and the
// automatic update, which restarts the API, waits for them (updateTick).
var jobLimits = map[string]time.Duration{
	"backup.run":     resticLimit + pruneLimit + time.Hour,
	"backup.restore": resticLimit + time.Hour,
	"migrate.run":    4 * agent.StreamLimit,
}

// seconds is d as the whole seconds an agent call's TimeoutSeconds takes.
func seconds(d time.Duration) int { return int(d / time.Second) }
