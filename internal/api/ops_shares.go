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
// keeps its home, the folder is bind-mounted into it by a systemd mount unit
// (for a guest with a shell into <www_root>/.shares/<guest>, with a symlink
// in the home: see shareBindFor), and ACLs on the folder let both accounts
// (and the web server) use the files. The guest sees the folder after
// signing in and nothing else.

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
// with (shell) or without a shell. systemd checks Where= and mount(8) then
// resolves it once more, following symlinks: whoever may rename an entry of
// a directory on the way in between could have the folder bound over
// /etc/systemd/system. The home of an SFTP-only guest is root's (the chroot
// sshd wants), so the mount point stays in it; the home of a guest with a
// shell is the guest's own, so the folder is mounted in .shares, root's, and
// the guest finds it in its home by a symlink. The agent refuses a mount
// point below anybody else's directory, and hands no directory on the way to
// one over while its unit exists.
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
	last, err := s.bindShare(ctx, sh, target, here)
	if err != nil {
		return nil, fail(err)
	}
	sh.Status, sh.LastError = last.Status.ActiveState, ""
	s.db.SetShareStatus(context.WithoutCancel(ctx), sh.ID, sh.Status, "") //nolint:errcheck // status is advisory
	return &apitypes.ShareStatus{Share: sh, Mounted: sh.Status == "active", Where: pl.where, MountPoint: here.mount, Target: target}, nil
}

// bindShare mounts target at b: the mount point (and for a guest with a
// shell the directories of .shares on the way, root's, the guest's own one
// readable by its group), the unit, enabled and started, and last the
// guest's symlink — a name of its home in the way fails the share before
// anything of the guest's is touched.
func (s *Server) bindShare(ctx context.Context, sh *store.Share, target string, b shareBind) (*agent.ServiceResponse, error) {
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
			return nil, fmt.Errorf("mount point: %w", err)
		}
	}
	unit := store.MountUnit(b.mount)
	text := fmt.Sprintf("# Generated by MonoPanel: shared folder of %s for %s\n[Unit]\nDescription=MonoPanel shared folder %s/%s for %s\nRequiresMountsFor=%s\n\n[Mount]\nWhat=%s\nWhere=%s\nType=none\nOptions=bind\n\n[Install]\nWantedBy=multi-user.target\n",
		sh.Domain, sh.Login, sh.Domain, sh.Path, sh.Login, path.Dir(target), target, b.mount)
	if _, err := s.agent.ApplyUnit(ctx, &agent.ApplyUnitRequest{Name: unit, Content: text, Origin: "share:" + unit}); err != nil {
		return nil, err
	}
	if _, err := s.agent.Service(ctx, "", "daemon-reload"); err != nil {
		return nil, err
	}
	var last *agent.ServiceResponse
	for _, action := range []string{"enable", "start"} {
		res, err := s.agent.Service(ctx, unit, action)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", action, unit, err)
		}
		last = res
	}
	if b.link != "" {
		s.clearStaleMountPoint(ctx, b.link)
		if err := s.agent.EnsureSymlink(ctx, &agent.EnsureSymlinkRequest{Path: b.link, Target: b.mount, Owner: sh.Login}); err != nil {
			return last, fmt.Errorf("the folder in the guest's home, %s (is the name taken there? pass another name): %w", b.link, err)
		}
	}
	return last, nil
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

// shareBound tells whether a unit mounts a folder at b or something is
// mounted there: a guest switched between shell and SFTP-only, or a panel up
// to 0.8.19, which mounted every folder in the guest's home, left it at the
// other place.
func (s *Server) shareBound(ctx context.Context, b shareBind) bool {
	res, err := s.agent.Service(ctx, store.MountUnit(b.mount), "status")
	if err != nil {
		return false
	}
	st := res.Status
	return (st.LoadState != "" && st.LoadState != "not-found") || st.ActiveState == "active"
}

// shareMounted tells whether a folder is mounted at b.
func (s *Server) shareMounted(ctx context.Context, b shareBind) bool {
	res, err := s.agent.Service(ctx, store.MountUnit(b.mount), "status")
	return err == nil && res.Status.ActiveState == "active"
}

// unbindShare takes the mount at b away: unmounted, the unit gone, then the
// empty mount point and, with link, the guest's symlink. The unit goes before
// the directories: while it exists the agent keeps the way to its mount point
// as it is.
//
// A folder in use stays mounted, and so does its unit: removed, it would
// leave a mount the panel no longer knows of. Not so when unsafe — a mount
// point in the home of a guest with a shell (a panel up to 0.8.19 put it
// there), which systemd would mount again at boot while the guest may rename
// what lies on the way, and the guest decides how long its folder stays in
// use: that unit goes all the same, and the error says that the folder stays
// mounted there until it is no longer in use (the next start of the panel
// tries again: the share is marked failed).
func (s *Server) unbindShare(ctx context.Context, b shareBind, link, unsafe bool) error {
	unit := store.MountUnit(b.mount)
	st, err := s.agent.Service(ctx, unit, "status")
	if err != nil || (st.Status.ActiveState != "inactive" && st.Status.ActiveState != "failed" && st.Status.ActiveState != "") {
		if _, serr := s.agent.Service(ctx, unit, "stop"); serr != nil && err == nil {
			if !unsafe {
				return fmt.Errorf("unmount %s: %w", b.mount, serr)
			}
			s.agent.Service(ctx, unit, "disable") //nolint:errcheck // may not be enabled
			if _, rerr := s.agent.RemoveUnit(ctx, &agent.RemoveUnitRequest{Name: unit}); rerr != nil {
				return fmt.Errorf("unmount %s: %w; its unit, in a home its account owns, is not removed either: %w", b.mount, serr, rerr)
			}
			return fmt.Errorf("unmount %s: %w; its unit is removed all the same, so that systemd never mounts it again in a home its account owns, and the folder stays mounted there until it is no longer in use: try again then (the panel tries again at its next start)", b.mount, serr)
		}
	}
	s.agent.Service(ctx, unit, "disable") //nolint:errcheck // may not be enabled
	// The agent has systemd forget the unit before it answers.
	if _, err := s.agent.RemoveUnit(ctx, &agent.RemoveUnitRequest{Name: unit}); err != nil {
		return err
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
	if res, err := s.agent.Service(ctx, store.MountUnit(here.mount), "status"); err == nil {
		out.Mounted = res.Status.ActiveState == "active"
	}
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

// refreshShareMounts moves the shared folders left at the other place: up
// to 0.8.19 every folder was mounted in the guest's home, also in the home a
// guest with a shell owns (shareBindFor), and a switch between shell and
// SFTP-only that stopped half way leaves one behind too. The agent, which
// starts first, has removed the units it found in a home their account owns
// (retireUnsafeShareUnits): a folder without a unit where it belongs is
// mounted there. Only a folder whose unit is still at the other place or
// missing at its own, or whose last apply failed (a move that found the
// folder in use), is touched: once done, this changes nothing.
func (s *Server) refreshShareMounts(ctx context.Context) {
	shares, err := s.db.ListShares(ctx, 0)
	if err != nil {
		return
	}
	for _, sh := range shares {
		pl, err := s.sharePlace(ctx, sh)
		if err != nil || pl.guest.UnixUID == nil {
			continue
		}
		if sh.Status != "failed" && !s.shareBound(ctx, s.shareBindFor(pl.guest, sh.Name, !pl.guest.Shell)) &&
			s.shareBound(ctx, s.shareBindFor(pl.guest, sh.Name, pl.guest.Shell)) {
			continue
		}
		st, err := s.applyShareAt(ctx, sh, pl, pl.guest.Shell, false)
		if err != nil {
			s.log.Warn("shared folder not moved", "guest", sh.Login, "name", sh.Name, "err", err)
			continue
		}
		s.log.Info("shared folder moved", "guest", sh.Login, "name", sh.Name, "mount_point", st.MountPoint)
	}
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
			return nil, huma.Error502BadGateway(err.Error())
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
			return nil, huma.Error502BadGateway(err.Error())
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
