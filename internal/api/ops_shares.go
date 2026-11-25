package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

// A shared folder: one account's site folder reachable by another account
// over SFTP. Changing the guest's home to a path inside the owner's site is
// what people ask for, and it cannot work — sshd wants the chroot owned by
// root and the panel wants every home under /var/www/<login>. So the guest
// keeps its home, the agent binds the folder into it (for a guest with a
// shell into <www_root>/.shares/<guest>, with a symlink in the home: see
// shareBindFor), and ACLs on the folder let both accounts (and the web
// server) use the files. The guest sees the folder after signing in and
// nothing else.
//
// The agent binds the folder it opened without following a symlink, in the
// host's mount namespace (internal/agent/sharebind.go), not a systemd mount
// unit by path: the folder's owner may swap it for a symlink, and a unit
// mounts whatever the path leads to by then. Nothing mounts a shared folder
// at boot; the panel binds them all again at its start (refreshShareMounts).

var sharePathRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

type shareListOutput struct {
	Body []*apitypes.ShareStatus
}

type shareOutput struct {
	Status int
	Body   *apitypes.ShareStatus
}

type shareCreateInput struct {
	Login string `path:"login"`
	Body  apitypes.ShareRequest
}

type shareNameInput struct {
	Login string `path:"login"`
	Name  string `path:"name"`
}

type shareUpdateInput struct {
	Login string `path:"login"`
	Name  string `path:"name"`
	Body  apitypes.ShareUpdate
}

// sharesDir is the directory of the web root where the shared folders of
// accounts with a shell are mounted, <www_root>/.shares/<guest>/<name> (the
// agent's sharesDirName): a login never begins with a dot, so no home is
// ever called that.
const sharesDir = ".shares"

// sharePlace is a share's folder and its guest.
type sharePlace struct {
	guest *store.User
	// target is the folder in the owner's site.
	target string
	// where is the folder as the guest sees it: <home>/<name>.
	where string
}

func (s *Server) sharePlace(ctx context.Context, sh *store.Share) (*sharePlace, error) {
	guest, err := s.db.GetUserByID(ctx, sh.UserID)
	if err != nil {
		return nil, err
	}
	site, err := s.db.GetSite(ctx, sh.SiteID)
	if err != nil {
		return nil, err
	}
	owner, err := s.db.GetUserByID(ctx, site.UserID)
	if err != nil {
		return nil, err
	}
	l := s.layoutFor(site, owner)
	return &sharePlace{guest: guest, target: path.Join(l.docroot, sh.Path), where: path.Join(s.userHome(guest), sh.Name)}, nil
}

func (s *Server) userHome(u *store.User) string {
	if u.Home != "" {
		return u.Home
	}
	return path.Join(s.cfg.WWWRoot, u.Login)
}

// shareBind is where a share's folder is mounted.
type shareBind struct {
	// mount is the mount point, the unit's Where=.
	mount string
	// link is the symlink in the home of a guest with a shell that leads to
	// the mount point; empty when the mount point is in the home itself.
	link string
}

// shareBindFor is where the folder name of guest is mounted for a guest
// with (shell) or without a shell. Whoever may rename an entry of a
// directory on the way to the mount point could have the folder bound over
// /etc/systemd/system. The home of an SFTP-only guest is root's (the chroot
// sshd wants), so the mount point stays in it; the home of a guest with a
// shell is the guest's own, so the folder is mounted in .shares, root's, and
// the guest finds it in its home by a symlink. The agent refuses a mount
// point below anybody else's directory, and hands no directory on the way to
// one over while it knows a shared folder there.
func (s *Server) shareBindFor(guest *store.User, name string, shell bool) shareBind {
	home := s.userHome(guest)
	if !shell {
		return shareBind{mount: path.Join(home, name)}
	}
	return shareBind{mount: path.Join(s.cfg.WWWRoot, sharesDir, guest.Login, name), link: path.Join(home, name)}
}

// applyShare makes the folder, its ACLs and the mount where the guest has it
// now. Every step is idempotent, so it also repairs a share after a reboot or
// mp site fix, and moves it when the guest switched between shell and
// SFTP-only.
func (s *Server) applyShare(ctx context.Context, sh *store.Share) (*apitypes.ShareStatus, error) {
	pl, err := s.sharePlace(ctx, sh)
	if err != nil {
		return nil, err
	}
	return s.applyShareAt(ctx, sh, pl, pl.guest.Shell, false)
}

// applyShareAt is applyShare for a guest with (shell) or without a shell: a
// mount at the other place goes first. For a guest with a shell that other
// place is in its home, which the guest owns: a unit there goes before
// anything else is tried, whatever fails afterwards, and never stays, in use
// or not (unbindShare) — unless homeRoots: the switch to a shell moves the
// folders while the home is root's still (moveUserShares).
func (s *Server) applyShareAt(ctx context.Context, sh *store.Share, pl *sharePlace, shell, homeRoots bool) (*apitypes.ShareStatus, error) {
	target := pl.target
	here, there := s.shareBindFor(pl.guest, sh.Name, shell), s.shareBindFor(pl.guest, sh.Name, !shell)
	fail := func(err error) error {
		s.db.SetShareStatus(context.WithoutCancel(ctx), sh.ID, "failed", err.Error()) //nolint:errcheck // the error being reported matters more
		return err
	}
	unsafe := shell && !homeRoots
	moveOut := func() error {
		if !s.shareBound(ctx, there) {
			return nil
		}
		if err := s.unbindShare(ctx, there, true, unsafe); err != nil {
			return fail(fmt.Errorf("moving it from %s: %w", there.mount, err))
		}
		return nil
	}
	if unsafe {
		if err := moveOut(); err != nil {
			return nil, err
		}
	}
	// The folder belongs to the site owner; setgid keeps the owner's group on
	// new files. The guest and the owner get rwx, inherited by everything
	// created inside, and the web group keeps reading as everywhere in the site.
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{{Path: target, Mode: 0o2770, Owner: sh.OwnerLogin, Group: sh.OwnerLogin}}}); err != nil {
		return nil, fail(fmt.Errorf("folder: %w", err))
	}
	entries := []string{"u:" + sh.Login + ":rwx", "u:" + sh.OwnerLogin + ":rwx", "g:" + s.cfg.WebGroup + ":r-x"}
	if err := s.agent.SetACL(ctx, &agent.SetACLRequest{Path: target, Entries: entries, Default: true, Recursive: true}); err != nil {
		return nil, fail(fmt.Errorf("acl: %w", err))
	}
	if !unsafe {
		if err := moveOut(); err != nil {
			return nil, err
		}
	}
	if err := s.bindShare(ctx, sh, target, here); err != nil {
		return nil, fail(err)
	}
	s.shareActive(ctx, sh)
	return &apitypes.ShareStatus{Share: sh, Mounted: true, Where: pl.where, MountPoint: here.mount, Target: target}, nil
}

// shareActive records that the folder is mounted where it belongs.
func (s *Server) shareActive(ctx context.Context, sh *store.Share) {
	sh.Status, sh.LastError = "active", ""
	s.db.SetShareStatus(context.WithoutCancel(ctx), sh.ID, sh.Status, "") //nolint:errcheck // status is advisory
}

// bindShare mounts target at b: the mount point (and for a guest with a
// shell the directories of .shares on the way, root's, the guest's own one
// readable by its group), the agent's bind of the folder, and last the
// guest's symlink — a name of its home in the way fails the share before
// anything of the guest's is touched.
func (s *Server) bindShare(ctx context.Context, sh *store.Share, target string, b shareBind) error {
	var dirs []agent.DirSpec
	if b.link != "" {
		dirs = append(dirs,
			agent.DirSpec{Path: path.Join(s.cfg.WWWRoot, sharesDir), Mode: 0o711, Owner: "root", Group: "root"},
			agent.DirSpec{Path: path.Dir(b.mount), Mode: 0o750, Owner: "root", Group: sh.Login})
	}
	// The mount point is root's; once mounted it shows the folder's own
	// ownership — and opens as the folder: a mode and an owner given to it
	// then would land on the site's folder (applied again by mp site fix,
	// mounted still by a move that could not unmount it).
	if !s.shareMounted(ctx, b) {
		dirs = append(dirs, agent.DirSpec{Path: b.mount, Mode: 0o755, Owner: "root", Group: "root"})
	}
	if len(dirs) > 0 {
		if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: dirs}); err != nil {
			return fmt.Errorf("mount point: %w", err)
		}
	}
	if _, err := s.agent.ShareBind(ctx, &agent.ShareBindRequest{What: target, Where: b.mount, Origin: "share:" + sh.Login + "/" + sh.Name}); err != nil {
		return err
	}
	if b.link != "" {
		s.clearStaleMountPoint(ctx, b.link)
		if _, err := s.agent.EnsureSymlink(ctx, &agent.EnsureSymlinkRequest{Path: b.link, Target: b.mount, Owner: sh.Login}); err != nil {
			return fmt.Errorf("the folder in the guest's home, %s (is the name taken there? pass another name): %w", b.link, err)
		}
	}
	return nil
}

// clearStaleMountPoint takes away the mount point a move out of the home of
// a guest with a shell left where the guest's symlink goes: the folder was in
// use and could not be unmounted (unbindShare), and once it is, an empty
// directory of root's remains. Nothing of the guest's goes — the guest makes
// no directory of root's, and the agent removes only an empty one.
func (s *Server) clearStaleMountPoint(ctx context.Context, p string) {
	res, err := s.agent.Stat(ctx, p)
	if err != nil || len(res.Entries) != 1 {
		return
	}
	if e := res.Entries[0]; !e.Exists || !e.IsDir || e.UID != 0 {
		return
	}
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{p}}); err != nil {
		s.log.Warn("share: an old mount point in the guest's home not removed", "path", p, "err", err)
	}
}

// shareState is what the agent finds at b's mount point.
func (s *Server) shareState(ctx context.Context, b shareBind) (agent.ShareMountState, bool) {
	res, err := s.agent.ShareStatus(ctx, b.mount)
	if err != nil || len(res.Entries) != 1 {
		return agent.ShareMountState{}, false
	}
	return res.Entries[0], true
}

// shareBound tells whether the agent knows a folder at b: mounted there,
// recorded as one it bound, or with a mount unit an older panel wrote. A
// guest switched between shell and SFTP-only, or a panel up to 0.8.19, which
// mounted every folder in the guest's home, left it at the other place.
func (s *Server) shareBound(ctx context.Context, b shareBind) bool {
	st, ok := s.shareState(ctx, b)
	return ok && (st.Mounted || st.Recorded || st.Unit)
}

// shareMounted tells whether a folder is mounted at b.
func (s *Server) shareMounted(ctx context.Context, b shareBind) bool {
	st, ok := s.shareState(ctx, b)
	return ok && st.Mounted
}

// unbindShare takes the mount at b away: the agent unmounts it and forgets
// it (a mount unit an older panel wrote for it goes first), then the empty
// mount point goes and, with link, the guest's symlink. The agent forgets
// the folder before the directories go: while it knows one there it keeps
// the way to its mount point as it is.
//
// A folder in use stays mounted, and the agent keeps knowing it. When unsafe
// — a mount point in the home of a guest with a shell, where a panel up to
// 0.8.19 put it, and the guest decides how long its folder stays in use —
// the error says that nothing mounts it there again and that it stays
// mounted until it is no longer in use (the next start of the panel tries
// again: the share is marked failed).
//
// Where the agent has nothing of a folder — nothing mounted, no record, no
// unit of an older panel's — it is not asked: the unbind would change
// nothing, and the agent refuses one where it knows no folder for a guest
// it no longer counts as a panel account (a member of a group of the
// system's, its system account gone), so that such a guest could never
// lose a share whose bind the agent refused — one a restore of the database
// brought back, a new one being cleaned up — nor one a removal that stopped
// half way unbound already, nor be removed itself.
func (s *Server) unbindShare(ctx context.Context, b shareBind, link, unsafe bool) error {
	if st, ok := s.shareState(ctx, b); !ok || st.Mounted || st.Recorded || st.Unit {
		if _, err := s.agent.ShareUnbind(ctx, &agent.ShareUnbindRequest{Where: b.mount}); err != nil {
			if unsafe {
				return fmt.Errorf("unmount %s: %w; nothing mounts it there again — no unit does, and the agent binds a folder only below directories of root's — and the folder stays mounted there until it is no longer in use: try again then (the panel tries again at its next start)", b.mount, err)
			}
			return fmt.Errorf("unmount %s: %w", b.mount, err)
		}
	}
	// The mount point is an empty root-owned directory once unmounted; a
	// leftover is only untidy, so it does not stop the removal.
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{b.mount}}); err != nil {
		s.log.Warn("share: mount point not removed", "path", b.mount, "err", err)
	}
	if b.link == "" {
		return nil
	}
	if link {
		if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{b.link}}); err != nil {
			s.log.Warn("share: the guest's symlink not removed", "path", b.link, "err", err)
		}
	}
	// The guest's directory in .shares goes with its last folder.
	s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{path.Dir(b.mount)}}) //nolint:errcheck // it still holds the guest's other folders
	return nil
}

// removeShare unmounts and forgets a share; the folder and its files stay
// with the site, only the guest's ACL entries go.
func (s *Server) removeShare(ctx context.Context, sh *store.Share) error {
	if err := s.dropShareMount(ctx, sh, true); err != nil {
		return err
	}
	return s.forgetShare(ctx, sh)
}

// dropShareMount takes a share's mount away wherever it is; link says
// whether the guest's symlink goes too (not after a failed creation: the name
// may be the guest's own).
func (s *Server) dropShareMount(ctx context.Context, sh *store.Share, link bool) error {
	pl, err := s.sharePlace(ctx, sh)
	if err != nil {
		return err
	}
	here, there := s.shareBindFor(pl.guest, sh.Name, pl.guest.Shell), s.shareBindFor(pl.guest, sh.Name, !pl.guest.Shell)
	if err := s.unbindShare(ctx, here, link, false); err != nil {
		return err
	}
	if s.shareBound(ctx, there) {
		// for a guest with a shell the other place is in its own home
		return s.unbindShare(ctx, there, link, pl.guest.Shell)
	}
	return nil
}

// forgetShare drops the guest's ACL entries from the folder and the share.
func (s *Server) forgetShare(ctx context.Context, sh *store.Share) error {
	if pl, err := s.sharePlace(ctx, sh); err == nil {
		if err := s.agent.SetACL(ctx, &agent.SetACLRequest{Path: pl.target, Remove: []string{"u:" + sh.Login}, Default: true, Recursive: true}); err != nil {
			s.log.Warn("share: guest ACL not removed", "path", pl.target, "guest", sh.Login, "err", err)
		}
	}
	return s.db.DeleteShare(ctx, sh.ID)
}

// shareStatus reports a share with its mount state.
func (s *Server) shareStatus(ctx context.Context, sh *store.Share) *apitypes.ShareStatus {
	out := &apitypes.ShareStatus{Share: sh}
	pl, err := s.sharePlace(ctx, sh)
	if err != nil {
		return out
	}
	here := s.shareBindFor(pl.guest, sh.Name, pl.guest.Shell)
	out.Target, out.Where, out.MountPoint = pl.target, pl.where, here.mount
	out.Mounted = s.shareMounted(ctx, here)
	return out
}

// moveUserShares mounts the guest's shared folders where an account with
// (shell) or without a shell has them, each taken from the other place
// first. The switch between the two (user.provision) calls it before the
// home becomes the account's and after it is root's again — the home is
// root's either way: a mount point never lies in a home its account owns,
// and the agent refuses that, too.
func (s *Server) moveUserShares(ctx context.Context, userID int64, shell bool, logf func(string, ...any)) error {
	if userID <= 0 {
		return nil
	}
	shares, err := s.db.ListShares(ctx, userID)
	if err != nil {
		return err
	}
	var errs []error
	for _, sh := range shares {
		pl, err := s.sharePlace(ctx, sh)
		if err != nil {
			errs = append(errs, fmt.Errorf("shared folder %s: %w", sh.Name, err))
			continue
		}
		st, err := s.applyShareAt(ctx, sh, pl, shell, true)
		if err != nil {
			errs = append(errs, fmt.Errorf("shared folder %s: %w", sh.Name, err))
			continue
		}
		logf("shared folder %s (%s/%s) mounted at %s", sh.Name, sh.Domain, sh.Path, st.MountPoint)
	}
	return errors.Join(errs...)
}

// refreshShareMounts mounts the shared folders at the panel's start: nothing
// mounts them at boot (the agent binds a folder from the directory it
// opened, never by a unit's path), so after a reboot every one is bound
// again here — the folder and its ACLs are as they were, only the bind is
// missing (bindShare). A folder left at the other place moves where it
// belongs: up to 0.8.19 every folder was mounted in the guest's home, also
// in the home a guest with a shell owns (shareBindFor), and a switch between
// shell and SFTP-only that stopped half way leaves one behind too; the agent,
// which starts first, has taken the units of the older panels over
// (retireUnsafeShareUnits). A folder mounted where it belongs, with nothing
// at the other place and its last apply not failed, is left as it is —
// unless the agent's record there names another folder than the share's:
// a restore of the database brought back a share whose name the guest was
// given again since, for another folder, say; the share's own is bound
// there instead. A folder the agent keeps that no share names any more goes
// first (unbindStrayShares).
func (s *Server) refreshShareMounts(ctx context.Context) {
	// The agent's records before the shares: a share made meanwhile is in
	// the database before its folder is bound and recorded, so the record
	// of none is taken for a stray.
	recs, rerr := s.agent.ShareRecords(ctx)
	shares, err := s.db.ListShares(ctx, 0)
	if err != nil {
		return
	}
	var kept map[string]string
	if rerr != nil {
		s.log.Warn("the agent does not list its records of the shared folders: a folder no share names any more stays bound", "err", rerr)
	} else {
		s.unbindStrayShares(ctx, shares, recs.Records)
		kept = keptFolders(recs.Records)
	}
	for _, sh := range shares {
		pl, err := s.sharePlace(ctx, sh)
		if err != nil || pl.guest.UnixUID == nil {
			continue
		}
		here := s.shareBindFor(pl.guest, sh.Name, pl.guest.Shell)
		elsewhere := s.shareBound(ctx, s.shareBindFor(pl.guest, sh.Name, !pl.guest.Shell))
		if sh.Status != "failed" && !elsewhere {
			other, another := keptOther(kept, here.mount, pl.target)
			if !another && s.shareMounted(ctx, here) {
				continue
			}
			if err := s.bindShare(ctx, sh, pl.target, here); err == nil {
				s.shareActive(ctx, sh)
				if another {
					s.log.Warn("a shared folder showed another folder than its share's: the share's own is bound there now", "guest", sh.Login, "name", sh.Name, "mount_point", here.mount, "folder", pl.target, "was", other)
				} else {
					s.log.Info("shared folder mounted", "guest", sh.Login, "name", sh.Name, "mount_point", here.mount)
				}
				continue
			}
			// whatever stood in the way, the whole apply finds it and says so
		}
		st, err := s.applyShareAt(ctx, sh, pl, pl.guest.Shell, false)
		if err != nil {
			s.log.Warn("shared folder not mounted", "guest", sh.Login, "name", sh.Name, "err", err)
			continue
		}
		s.log.Info("shared folder applied", "guest", sh.Login, "name", sh.Name, "mount_point", st.MountPoint)
	}
}

// keptFolders maps the mount points of the agent's records to the folders
// they name (a record that cannot be read names none).
func keptFolders(recs []agent.ShareRecordState) map[string]string {
	kept := map[string]string{}
	for _, r := range recs {
		if r.What != "" {
			kept[r.Where] = r.What
		}
	}
	return kept
}

// keptOther returns the folder the agent's record at mount names (kept,
// keptFolders) when it is not target, the folder of the share that names the
// mount point: what the agent bound there is not the share's. A rename of
// the same directory since reads as another too; binding the share's folder
// changes nothing then but the record.
func keptOther(kept map[string]string, mount, target string) (string, bool) {
	what, ok := kept[mount]
	return what, ok && what != target
}

// nameShare adds to named the mount points of guest's shared folder name:
// in the guest's home and in .shares, whichever the guest has it at now — a
// folder left at the other place is the start-up refresh's to move.
func (s *Server) nameShare(named map[string]bool, guest *store.User, name string) {
	for _, shell := range []bool{false, true} {
		named[s.shareBindFor(guest, name, shell).mount] = true
	}
}

// namedMountPoints are the mount points the shares name (nameShare); an
// error when the place of a share cannot be told: then no record may pass
// for a stray.
func (s *Server) namedMountPoints(ctx context.Context, shares []*store.Share) (map[string]bool, error) {
	named := map[string]bool{}
	for _, sh := range shares {
		pl, err := s.sharePlace(ctx, sh)
		if err != nil {
			return nil, fmt.Errorf("shared folder %s of %s: %w", sh.Name, sh.Login, err)
		}
		s.nameShare(named, pl.guest, sh.Name)
	}
	return named, nil
}

// unbindStrayShares unbinds, at the panel's start, every shared folder the
// agent keeps a record of whose mount point no share of the database names:
// one a restore of the database lost, say. Such a record keeps the way to
// its mount point root's (409 "remove or move the shared folders of <login>
// first"), and nothing else in the panel would ever remove it. A folder in
// use stays bound and recorded: the next start tries again, and the doctor
// names it meanwhile (shareDoctorCheck).
func (s *Server) unbindStrayShares(ctx context.Context, shares []*store.Share, recs []agent.ShareRecordState) {
	named, err := s.namedMountPoints(ctx, shares)
	if err != nil {
		s.log.Warn("the shared folders no share names any more are not looked for: the panel does not know where a share's folder is mounted", "err", err)
		return
	}
	for _, r := range recs {
		if named[r.Where] {
			s.strayShares.Delete(r.Where)
			continue
		}
		if err := s.unbindStray(ctx, r.Where); err != nil {
			s.strayShares.Store(r.Where, err)
			s.log.Warn("a shared folder the panel no longer knows was not unbound: the panel tries again at its next start", "mount_point", r.Where, "folder", r.What, "err", err)
			continue
		}
		s.strayShares.Delete(r.Where)
		s.log.Info("a shared folder the panel no longer knows was unbound", "mount_point", r.Where, "folder", r.What)
	}
}

// unbindStray takes away the shared folder at the mount point where that no
// share names: the agent unmounts it and forgets it, the empty mount point
// goes and, in .shares, the guest's directory with its last folder. A
// symlink in the home of a guest with a shell that led to it stays — no
// share tells the panel it put one there — and leads nowhere now.
func (s *Server) unbindStray(ctx context.Context, where string) error {
	if err := s.unbindShare(ctx, shareBind{mount: where}, false, false); err != nil {
		return err
	}
	if dir := path.Dir(where); path.Dir(dir) == path.Join(s.cfg.WWWRoot, sharesDir) {
		s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{dir}}) //nolint:errcheck // it still holds the guest's other folders
	}
	return nil
}

// strayShareNotes names the shared folders the agent still keeps that no
// share names — recs are its records, named the mount points of the shares
// (nameShare) — with why the start-up refresh left them (unbindStrayShares):
// in use, mostly.
func (s *Server) strayShareNotes(recs []agent.ShareRecordState, named map[string]bool) []string {
	var out []string
	for _, r := range recs {
		if named[r.Where] {
			continue
		}
		why := "not unbound yet"
		if v, ok := s.strayShares.Load(r.Where); ok {
			var ae *agent.Error
			if err, _ := v.(error); errors.As(err, &ae) && ae.Status == http.StatusConflict {
				why = "in use"
			} else if err != nil {
				why = err.Error()
			}
		}
		out = append(out, r.Where+": "+why)
	}
	return out
}

// shareDoctorCheck is the doctor's line on the shared folders: is each one
// mounted where its guest has it? Nothing mounts them at boot — the panel
// binds them at its start — and a folder the agent refuses to bind (its
// owner swapped it for a symlink, say) stays unmounted: the line says which,
// and why — as it does one where the agent keeps another folder than the
// share's (refreshShareMounts binds the share's own there). It also names a
// folder the agent still keeps that no share names any more: the panel's
// start unbinds those, all but one in use, say. nil without either.
func (s *Server) shareDoctorCheck(ctx context.Context) *apitypes.Check {
	// The records before the shares, as at the start (refreshShareMounts).
	var recs []agent.ShareRecordState
	if res, err := s.agent.ShareRecords(ctx); err == nil {
		recs = res.Records
	}
	shares, err := s.db.ListShares(ctx, 0)
	if err != nil {
		return nil
	}
	kept := keptFolders(recs)
	var points, targets []string
	var listed []*store.Share
	named, placed := map[string]bool{}, true
	for _, sh := range shares {
		pl, err := s.sharePlace(ctx, sh)
		if err != nil {
			placed = false
			continue
		}
		s.nameShare(named, pl.guest, sh.Name)
		if pl.guest.UnixUID == nil {
			continue
		}
		points = append(points, s.shareBindFor(pl.guest, sh.Name, pl.guest.Shell).mount)
		targets = append(targets, pl.target)
		listed = append(listed, sh)
	}
	var strays []string
	if placed {
		strays = s.strayShareNotes(recs, named)
	}
	if len(points) == 0 && len(strays) == 0 {
		return nil
	}
	status, parts := "ok", []string{}
	if len(points) > 0 {
		res, err := s.agent.ShareStatus(ctx, points...)
		if err != nil || len(res.Entries) != len(points) {
			c := check("shared folders", "warn", fmt.Sprintf("the agent does not say whether they are mounted: %v", err))
			return &c
		}
		var missing []string
		for i, e := range res.Entries {
			sh := listed[i]
			var why string
			switch other, another := keptOther(kept, points[i], targets[i]); {
			case !e.Mounted:
				why = "not mounted"
				if sh.LastError != "" {
					why = sh.LastError
				}
			case another:
				why = "another folder is mounted there, " + other
				if sh.LastError != "" {
					why += " (" + sh.LastError + ")"
				}
			default:
				continue
			}
			missing = append(missing, fmt.Sprintf("%s of %s (%s/%s): %s", sh.Name, sh.Login, sh.Domain, sh.Path, why))
		}
		if len(missing) == 0 {
			parts = append(parts, fmt.Sprintf("%d mounted", len(points)))
		} else {
			status = "warn"
			parts = append(parts, fmt.Sprintf("%d of %d not mounted — %s; mp site fix <the site's domain> mounts a site's folders again", len(missing), len(points), strings.Join(missing, "; ")))
		}
	}
	if len(strays) > 0 {
		status = "warn"
		parts = append(parts, fmt.Sprintf("%d kept by the agent that no shared folder of the panel names — %s; the panel unbinds such a folder at its start once nothing uses it (systemctl restart monopanel-api)", len(strays), strings.Join(strays, "; ")))
	}
	c := check("shared folders", status, strings.Join(parts, "; "))
	return &c
}

// siteNoPHPPaths lists the shared folders of a site where PHP must not run.
func (s *Server) siteNoPHPPaths(ctx context.Context, siteID int64) []string {
	shares, err := s.db.ListSiteShares(ctx, siteID)
	if err != nil {
		return nil
	}
	var out []string
	for _, sh := range shares {
		if sh.NoPHP {
			out = append(out, sh.Path)
		}
	}
	return out
}

// reapplyShareACLs restores the guests' ACLs after mp site fix rewrote the
// site tree's entries.
func (s *Server) reapplyShareACLs(ctx context.Context, jc *jobs.Context, siteID int64) {
	shares, err := s.db.ListSiteShares(ctx, siteID)
	if err != nil {
		return
	}
	for _, sh := range shares {
		if _, err := s.applyShare(ctx, sh); err != nil {
			jc.Logf("warning: shared folder %s for %s: %v", sh.Path, sh.Login, err)
			continue
		}
		jc.Logf("shared folder %s for %s: ACLs and mount restored", sh.Path, sh.Login)
	}
}

// removeSiteShares unmounts every guest's view of a site before the site
// goes; the mounts would otherwise keep the purged folder alive.
func (s *Server) removeSiteShares(ctx context.Context, jc *jobs.Context, siteID int64) error {
	shares, err := s.db.ListSiteShares(ctx, siteID)
	if err != nil {
		return err
	}
	entry := false
	for _, sh := range shares {
		if err := s.removeShare(ctx, sh); err != nil {
			return fmt.Errorf("shared folder %s for %s: %w", sh.Path, sh.Login, err)
		}
		jc.Logf("shared folder %s for %s removed", sh.Path, sh.Login)
		entry = entry || sh.Entry
	}
	if entry {
		if err := s.ensureSSHConfig(ctx); err != nil {
			jc.Logf("warning: sshd drop-in not refreshed: %v", err)
		}
	}
	return nil
}

// removeUserShares unmounts the folders a guest reaches before its home goes;
// a site that denied PHP in such a folder is re-applied without the rule.
func (s *Server) removeUserShares(ctx context.Context, jc *jobs.Context, userID int64) error {
	shares, err := s.db.ListShares(ctx, userID)
	if err != nil {
		return err
	}
	reapply := map[int64]string{}
	entry := false
	for _, sh := range shares {
		if err := s.removeShare(ctx, sh); err != nil {
			return fmt.Errorf("shared folder %s: %w", sh.Name, err)
		}
		jc.Logf("shared folder %s (%s/%s) removed", sh.Name, sh.Domain, sh.Path)
		if sh.NoPHP {
			reapply[sh.SiteID] = sh.Domain
		}
		entry = entry || sh.Entry
	}
	if entry {
		if err := s.ensureSSHConfig(ctx); err != nil {
			jc.Logf("warning: sshd drop-in not refreshed: %v", err)
		}
	}
	for id, domain := range reapply {
		if _, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: id}, jobs.WithLockKey("site:"+domain), jobs.WithRequestedBy("system")); err != nil {
			jc.Logf("warning: site %s not re-applied after the shared folder went: %v", domain, err)
		}
	}
	return nil
}

func (s *Server) registerShares() {
	huma.Register(s.api, huma.Operation{
		OperationID: "shares-list", Method: http.MethodGet, Path: "/users/{login}/shares", Summary: "Folders of other accounts' sites this account reaches over SFTP", Tags: []string{"users"}, Security: secured,
	}, func(ctx context.Context, in *loginInputPath) (*shareListOutput, error) {
		u, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		shares, err := s.db.ListShares(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		out := make([]*apitypes.ShareStatus, 0, len(shares))
		for _, sh := range shares {
			out = append(out, s.shareStatus(ctx, sh))
		}
		return &shareListOutput{Body: out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "shares-create", Method: http.MethodPost, Path: "/users/{login}/shares", Summary: "Give the account a folder of another account's site: bind mount into its home plus ACLs (admin)", Tags: []string{"users"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *shareCreateInput) (*shareOutput, error) {
		p := principalFrom(ctx)
		guest, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		if guest.UnixUID == nil || guest.Status != store.UserActive {
			return nil, huma.Error422UnprocessableEntity("the guest must be an active user with a provisioned unix account")
		}
		site, err := s.db.GetSiteByDomain(ctx, strings.ToLower(strings.TrimSpace(in.Body.Site)))
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error422UnprocessableEntity("site not found")
		} else if err != nil {
			return nil, err
		}
		if site.UserID == guest.ID {
			return nil, huma.Error422UnprocessableEntity("the site already belongs to this account")
		}
		// The path is taken as written: cleaning "../x" into "x" would hide
		// an attempt to leave the docroot, so it is refused instead.
		rel := strings.Trim(in.Body.Path, "/")
		if rel == "" || strings.Contains(rel, "..") || path.Clean(rel) != rel || !sharePathRe.MatchString(rel) {
			return nil, huma.Error422UnprocessableEntity("path must be a folder inside the site, like kaspy or upload/exchange")
		}
		name := in.Body.Name
		if name == "" {
			name = strings.ToLower(path.Base(rel))
		}
		if !appNameRe.MatchString(name) {
			return nil, huma.Error422UnprocessableEntity("name must be lowercase letters, digits, - or _ (pass name explicitly)")
		}
		if name == "data" || name == ".ssh" {
			return nil, huma.Error422UnprocessableEntity("name " + name + " is taken by the account's own layout")
		}
		owner, err := s.db.GetUserByID(ctx, site.UserID)
		if err != nil {
			return nil, err
		}
		if in.Body.Entry && guest.Shell {
			return nil, huma.Error422UnprocessableEntity("an entry folder is for SFTP-only accounts; an account with a shell signs in wherever it likes")
		}
		sh := &store.Share{UserID: guest.ID, Login: guest.Login, SiteID: site.ID, Domain: site.Domain, OwnerLogin: owner.Login, Path: rel, Name: name, NoPHP: in.Body.NoPHP, Entry: in.Body.Entry}
		if err := s.db.CreateShare(ctx, sh); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("the account already has a shared folder named " + name)
			}
			return nil, err
		}
		st, err := s.applyShare(ctx, sh)
		if err != nil {
			// What the attempt made goes again; a name in the guest's home is
			// left alone, it may be the guest's own.
			cctx := context.WithoutCancel(ctx)
			if derr := s.dropShareMount(cctx, sh, false); derr != nil {
				s.log.Warn("share: mount of a failed share not removed", "guest", sh.Login, "name", sh.Name, "err", derr)
			}
			if derr := s.forgetShare(cctx, sh); derr != nil {
				s.db.DeleteShare(cctx, sh.ID) //nolint:errcheck // nothing to keep
			}
			return nil, agentStatus(err.Error(), err)
		}
		if sh.Entry {
			if err := s.ensureSSHConfig(ctx); err != nil {
				return nil, huma.Error502BadGateway("sshd: " + err.Error())
			}
		}
		if sh.NoPHP {
			job, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: site.ID}, jobs.WithLockKey("site:"+site.Domain), jobs.WithRequestedBy(p.Login))
			if err != nil {
				return nil, err
			}
			st.JobID = job.ID
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "share.create", Target: guest.Login, IP: requestInfo(ctx).IP, Details: map[string]any{"site": site.Domain, "path": rel, "name": name, "no_php": sh.NoPHP, "entry": sh.Entry}})
		return &shareOutput{Status: http.StatusCreated, Body: st}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "shares-update", Method: http.MethodPatch, Path: "/users/{login}/shares/{name}", Summary: "Make the folder the account's SFTP entry point, or stop that: the session starts inside the folder instead of the home (admin)", Tags: []string{"users"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *shareUpdateInput) (*shareOutput, error) {
		p := principalFrom(ctx)
		guest, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		sh, err := s.db.GetShare(ctx, guest.ID, in.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("shared folder not found")
		} else if err != nil {
			return nil, err
		}
		if in.Body.Entry && guest.Shell {
			return nil, huma.Error422UnprocessableEntity("an entry folder is for SFTP-only accounts; an account with a shell signs in wherever it likes")
		}
		if err := s.db.SetShareEntry(ctx, sh, in.Body.Entry); err != nil {
			return nil, err
		}
		if err := s.ensureSSHConfig(ctx); err != nil {
			return nil, huma.Error502BadGateway("sshd: " + err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "share.update", Target: guest.Login, IP: requestInfo(ctx).IP, Details: map[string]any{"name": sh.Name, "entry": sh.Entry}})
		return &shareOutput{Status: http.StatusOK, Body: s.shareStatus(ctx, sh)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "shares-delete", Method: http.MethodDelete, Path: "/users/{login}/shares/{name}", Summary: "Take the folder away: unmount and drop the guest's ACLs; the files stay with the site (admin)", Tags: []string{"users"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *shareNameInput) (*shareOutput, error) {
		p := principalFrom(ctx)
		guest, err := s.loadUserFor(ctx, in.Login)
		if err != nil {
			return nil, err
		}
		sh, err := s.db.GetShare(ctx, guest.ID, in.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("shared folder not found")
		} else if err != nil {
			return nil, err
		}
		if err := s.removeShare(ctx, sh); err != nil {
			return nil, agentStatus(err.Error(), err)
		}
		if sh.Entry {
			if err := s.ensureSSHConfig(ctx); err != nil {
				return nil, huma.Error502BadGateway("sshd: " + err.Error())
			}
		}
		st := &apitypes.ShareStatus{Share: sh}
		if sh.NoPHP {
			job, err := s.jobs.Enqueue(ctx, "site.apply", sitePayload{SiteID: sh.SiteID}, jobs.WithLockKey("site:"+sh.Domain), jobs.WithRequestedBy(p.Login))
			if err != nil {
				return nil, err
			}
			st.JobID = job.ID
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "share.delete", Target: guest.Login, IP: requestInfo(ctx).IP, Details: map[string]any{"site": sh.Domain, "path": sh.Path, "name": sh.Name}})
		return &shareOutput{Status: http.StatusOK, Body: st}, nil
	})
}
