// Package retention decides which off-site database backups to keep.
//
// Backups live in the workspace bucket as databases/<database>/<backup>.dump
// (under the bucket's configured prefix). Scheduled and manual backups each
// get their own key; the pre-deployment backup overwrites
// databases/<database>/predeployment.dump. Select applies a
// grandfather-father-son policy to the first kind and never touches the
// second. Every function here is pure: callers list the bucket, pass the
// objects in, and delete what the plan says.
package retention

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Limits of a policy. A year of dailies, five years of weeklies and ten
// years of monthlies are more than any installation should need.
const (
	MaxDaily          = 365
	MaxWeekly         = 260
	MaxMonthly        = 120
	MaxOrphanGrace    = 365
	DefaultDaily      = 7
	DefaultWeekly     = 4
	DefaultMonthly    = 0
	DefaultOrphanDays = 30
	// Recent protects objects younger than this unless a newer backup of the
	// same day supersedes them.
	Recent = 24 * time.Hour
	// Predeployment is the one overwritten pre-deployment object name.
	Predeployment = "predeployment"
)

// Policy is how many scheduled backups of one database stay off-site: the
// newest backup of each of the Daily most recent days, the Weekly most
// recent ISO weeks and the Monthly most recent months that have backups.
// Days, weeks and months are UTC calendar periods.
type Policy struct {
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

// Default is the policy an installation starts with.
func Default() Policy {
	return Policy{Daily: DefaultDaily, Weekly: DefaultWeekly, Monthly: DefaultMonthly}
}

// Validate refuses counts outside the supported range.
func (p Policy) Validate() error {
	if p.Daily < 0 || p.Daily > MaxDaily {
		return fmt.Errorf("keep between 0 and %d daily backups", MaxDaily)
	}
	if p.Weekly < 0 || p.Weekly > MaxWeekly {
		return fmt.Errorf("keep between 0 and %d weekly backups", MaxWeekly)
	}
	if p.Monthly < 0 || p.Monthly > MaxMonthly {
		return fmt.Errorf("keep between 0 and %d monthly backups", MaxMonthly)
	}
	return nil
}

// String is a short description for logs and plans.
func (p Policy) String() string {
	return fmt.Sprintf("%d daily, %d weekly, %d monthly", p.Daily, p.Weekly, p.Monthly)
}

// Settings is a workspace's retention: the default policy of every
// database, how long backups of a removed database stay, and whether
// cleanup runs on its own after uploads.
type Settings struct {
	Policy
	OrphanGraceDays int  `json:"orphan_grace_days"`
	Automatic       bool `json:"automatic"`
}

// DefaultSettings are the settings of a new installation.
func DefaultSettings() Settings {
	return Settings{Policy: Default(), OrphanGraceDays: DefaultOrphanDays, Automatic: true}
}

// Validate checks the policy and the orphan grace period.
func (s Settings) Validate() error {
	if err := s.Policy.Validate(); err != nil {
		return err
	}
	if s.OrphanGraceDays < 1 || s.OrphanGraceDays > MaxOrphanGrace {
		return fmt.Errorf("keep backups of removed databases between 1 and %d days", MaxOrphanGrace)
	}
	return nil
}

// OrphanGrace is the grace period as a duration.
func (s Settings) OrphanGrace() time.Duration {
	return time.Duration(s.OrphanGraceDays) * 24 * time.Hour
}

var (
	databasePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	namePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// Prefix is the folder of one database's backups, relative to the bucket's
// configured prefix.
func Prefix(database string) string { return "databases/" + database + "/" }

// ParseKey splits databases/<database>/<name>.dump. Any other key belongs to
// something else and is never planned for deletion.
func ParseKey(key string) (database, name string, ok bool) {
	if path.Clean(key) != key || strings.ContainsAny(key, "\\\r\n") {
		return "", "", false
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "databases" || !databasePattern.MatchString(parts[1]) || !strings.HasSuffix(parts[2], ".dump") {
		return "", "", false
	}
	name = strings.TrimSuffix(parts[2], ".dump")
	if !namePattern.MatchString(name) {
		return "", "", false
	}
	return parts[1], name, true
}

// Object is one backup object. Created is the backup record's time when the
// caller knows it, otherwise the object's last-modified time. Location
// distinguishes copies of the same key under another bucket prefix (an
// older addressing mode); it is empty for the current prefix.
type Object struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
	Location string    `json:"location,omitempty"`
}

// Entry is an object with the reason it is kept or deleted.
type Entry struct {
	Object
	Reason string `json:"reason"`
}

// Database plan statuses.
const (
	StatusRetained  = "retained"  // a known database; the policy applies
	StatusProtected = "protected" // an operation or move is using it
	StatusOrphaned  = "orphaned"  // no server has the database any more
	StatusUnknown   = "unknown"   // the owner could not be determined
)

// DatabasePlan is the decision for one database's objects.
type DatabasePlan struct {
	Database    string     `json:"database"`
	Status      string     `json:"status"`
	Policy      Policy     `json:"policy"`
	Override    bool       `json:"override,omitempty"`
	Removed     *time.Time `json:"removed,omitempty"`
	DeleteAfter *time.Time `json:"delete_after,omitempty"`
	Note        string     `json:"note,omitempty"`
	Keep        []Entry    `json:"keep"`
	Delete      []Entry    `json:"delete"`
	KeepBytes   int64      `json:"keep_bytes"`
	DeleteBytes int64      `json:"delete_bytes"`
}

// Plan is a reviewed cleanup: every database's decision, totals, and a
// fingerprint of exactly what would be deleted.
type Plan struct {
	Databases     []DatabasePlan `json:"databases"`
	KeepObjects   int            `json:"keep_objects"`
	KeepBytes     int64          `json:"keep_bytes"`
	DeleteObjects int            `json:"delete_objects"`
	DeleteBytes   int64          `json:"delete_bytes"`
	Fingerprint   string         `json:"fingerprint"`
	Generated     time.Time      `json:"generated"`
}

// owned returns the objects of database, dropping keys outside its folder.
func owned(database string, objects []Object) []Object {
	out := []Object{}
	for _, o := range objects {
		if db, _, ok := ParseKey(o.Key); ok && db == database {
			out = append(out, o)
		}
	}
	return out
}

func newest(a, b Object) int {
	if c := b.Created.Compare(a.Created); c != 0 {
		return c
	}
	if c := strings.Compare(b.Key, a.Key); c != 0 {
		return c
	}
	return strings.Compare(a.Location, b.Location)
}

// Select plans one database's objects under policy at now. protected holds
// keys a running move or restore needs. The rules, in order:
//
//   - keys outside databases/<database>/ are ignored entirely;
//   - the pre-deployment copy is always kept and never counted;
//   - the newest nonempty backup is always kept;
//   - protected keys are kept;
//   - the newest backup of each of the Daily most recent days, Weekly most
//     recent ISO weeks and Monthly most recent months with backups is kept
//     (gaps without backups do not use up the count);
//   - an object younger than Recent is kept unless a newer backup of the
//     same UTC day supersedes it;
//   - everything else is deleted.
func Select(database string, objects []Object, policy Policy, now time.Time, protected map[string]bool) DatabasePlan {
	plan := DatabasePlan{Database: database, Status: StatusRetained, Policy: policy, Keep: []Entry{}, Delete: []Entry{}}
	var candidates []Object
	for _, o := range owned(database, objects) {
		if _, name, _ := ParseKey(o.Key); name == Predeployment {
			plan.keep(o, "pre-deployment copy")
			continue
		}
		candidates = append(candidates, o)
	}
	slices.SortFunc(candidates, newest)
	reasons := make([][]string, len(candidates))
	for i, o := range candidates {
		if o.Size > 0 {
			reasons[i] = append(reasons[i], "newest backup")
			break
		}
	}
	for i, o := range candidates {
		if protected[o.Key] {
			reasons[i] = append(reasons[i], "used by a running move or restore")
		}
	}
	bucket := func(count int, label string, period func(time.Time) string) {
		seen := map[string]bool{}
		for i, o := range candidates {
			if count <= 0 || o.Size <= 0 {
				continue
			}
			p := period(o.Created.UTC())
			if seen[p] {
				continue
			}
			if len(seen) == count {
				return
			}
			seen[p] = true
			reasons[i] = append(reasons[i], label+" "+p)
		}
	}
	bucket(policy.Daily, "daily", func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(policy.Weekly, "weekly", func(t time.Time) string {
		year, week := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	})
	bucket(policy.Monthly, "monthly", func(t time.Time) string { return t.Format("2006-01") })
	days := map[string]bool{}
	for i, o := range candidates {
		day := o.Created.UTC().Format("2006-01-02")
		superseded := days[day]
		days[day] = true
		if now.Sub(o.Created) < Recent && !superseded {
			reasons[i] = append(reasons[i], "less than 24 hours old")
		}
	}
	for i, o := range candidates {
		if len(reasons[i]) > 0 {
			plan.keep(o, strings.Join(reasons[i], ", "))
		} else {
			plan.remove(o, "outside retention ("+policy.String()+")")
		}
	}
	return plan
}

// KeepAll plans every object of database as kept, with one reason.
func KeepAll(database, status, reason string, objects []Object) DatabasePlan {
	plan := DatabasePlan{Database: database, Status: status, Note: reason, Keep: []Entry{}, Delete: []Entry{}}
	for _, o := range owned(database, objects) {
		plan.keep(o, reason)
	}
	return plan
}

// Orphan plans the objects of a database no server has any more. They stay
// for grace after removed; then every object of its folder, including the
// pre-deployment copy, is deleted.
func Orphan(database string, objects []Object, removed time.Time, grace time.Duration, now time.Time) DatabasePlan {
	removed = removed.UTC()
	after := removed.Add(grace)
	plan := DatabasePlan{Database: database, Status: StatusOrphaned, Removed: &removed, DeleteAfter: &after, Keep: []Entry{}, Delete: []Entry{}}
	label := "orphaned (removed " + removed.Format("2006-01-02") + ")"
	for _, o := range owned(database, objects) {
		if now.Before(after) {
			plan.keep(o, label+"; kept until "+after.Format("2006-01-02"))
		} else {
			plan.remove(o, label+"; grace period ended "+after.Format("2006-01-02"))
		}
	}
	plan.Note = label
	return plan
}

func (p *DatabasePlan) keep(o Object, reason string) {
	p.Keep = append(p.Keep, Entry{Object: o, Reason: reason})
	p.KeepBytes += o.Size
}

func (p *DatabasePlan) remove(o Object, reason string) {
	p.Delete = append(p.Delete, Entry{Object: o, Reason: reason})
	p.DeleteBytes += o.Size
}

// Summarize orders database plans, totals them and fingerprints the
// deletions.
func Summarize(databases []DatabasePlan, generated time.Time) Plan {
	plan := Plan{Databases: append([]DatabasePlan{}, databases...), Generated: generated.UTC()}
	slices.SortFunc(plan.Databases, func(a, b DatabasePlan) int { return strings.Compare(a.Database, b.Database) })
	var lines []string
	for _, d := range plan.Databases {
		plan.KeepObjects += len(d.Keep)
		plan.KeepBytes += d.KeepBytes
		plan.DeleteObjects += len(d.Delete)
		plan.DeleteBytes += d.DeleteBytes
		for _, e := range d.Delete {
			lines = append(lines, fmt.Sprintf("%s\x00%s\x00%d\x00%d\n", e.Location, e.Key, e.Size, e.Created.UnixNano()))
		}
	}
	slices.Sort(lines)
	hash := sha256.New()
	for _, line := range lines {
		hash.Write([]byte(line))
	}
	plan.Fingerprint = hex.EncodeToString(hash.Sum(nil))
	return plan
}

// ErrPlanChanged refuses to apply a plan that differs from the reviewed one.
var ErrPlanChanged = errors.New("the cleanup plan changed since it was reviewed; review it again")

// Check compares a freshly computed plan with the reviewed fingerprint.
func (p Plan) Check(fingerprint string) error {
	if fingerprint == "" || fingerprint != p.Fingerprint {
		return ErrPlanChanged
	}
	return nil
}
