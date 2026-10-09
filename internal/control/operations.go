package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/mail"
	"net/http"
	"time"
)

type Incident struct {
	Key      string    `json:"key"`
	Message  string    `json:"message"`
	Active   bool      `json:"active"`
	Failures int       `json:"failures"`
	Notified bool      `json:"notified"`
	Updated  time.Time `json:"updated"`
}

func (c *Control) observe(ctx context.Context, key, message string, failed bool, threshold int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.Incidents == nil {
		c.data.Incidents = map[string]Incident{}
	}
	old := c.data.Incidents[key]
	next := old
	next.Key = key
	next.Message = message

	if failed {
		if next.Failures < threshold {
			next.Failures++
		}
		if next.Failures >= threshold {
			next.Active = true
		}
	} else {
		next.Failures = 0
		next.Active = false
	}
	needsNotification := configuredMail() && ((next.Active && !old.Notified) || (!next.Active && old.Notified))
	if !needsNotification && next.Active == old.Active && next.Failures == old.Failures && next.Notified == old.Notified && (old.Key == "" || next.Message == old.Message) {
		return nil
	}
	next.Updated = time.Now().UTC()
	if needsNotification && ((next.Active && !old.Notified) || (!next.Active && old.Notified)) {
		subject := "Līdza Deploy alert: " + message
		text := message + "\n\nOpen " + c.cfg.PublicURL + "/console.html for details."
		if !next.Active {
			subject = "Līdza Deploy recovery: " + message
			text = "Recovered: " + text
		}
		if _, err := mail.From(ctx).Send(ctx, mail.Message{To: c.cfg.User, Subject: subject, Text: text}); err != nil {
			return errors.New("could not queue operational alert")
		}
		next.Notified = next.Active
	}
	// Bound stale, recovered fingerprints while keeping active incidents visible.
	if len(c.data.Incidents) > 2000 {
		for k, v := range c.data.Incidents {
			if !v.Active && time.Since(v.Updated) > 7*24*time.Hour {
				delete(c.data.Incidents, k)
			}
		}
	}
	c.data.Incidents[key] = next
	if err := c.save(); err != nil {
		c.data.Incidents[key] = old
		return err
	}
	return nil
}
func (c *Control) operationsTick(ctx context.Context, _ json.RawMessage) error {
	request, _ := http.NewRequestWithContext(ctx, "GET", c.cfg.PublicURL, nil)
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	record(c.securityTick(ctx))
	for _, result := range c.readFleet(request, "/v1/databases") {
		record(c.observe(ctx, "server:"+result.Server.ID, "Hosting server "+result.Server.Name+" is unreachable", result.Err != nil, 3))
		if result.Err != nil {
			continue
		}
		var databases []agent.DatabaseView
		if json.Unmarshal(result.Body, &databases) != nil {
			continue
		}
		for _, d := range databases {
			if d.Retained {
				continue
			}
			if d.Ready && d.Backup.Hours > 0 {
				var latest time.Time
				for _, b := range d.Backups {
					if b.Kind != "predeployment" && b.Created.After(latest) {
						latest = b.Created
					}
				}
				stale := latest.IsZero() && d.NextBackup.Before(time.Now().Add(-30*time.Minute)) || !latest.IsZero() && time.Since(latest) > time.Duration(d.Backup.Hours)*2*time.Hour
				record(c.observe(ctx, "backup-age:"+result.Server.ID+":"+d.AppID, "Scheduled backup is overdue for "+d.AppID, stale, 2))
			}
			record(c.observe(ctx, "database:"+result.Server.ID+":"+d.AppID, "Database or backup operation failed for "+d.AppID, d.Error != "", 1))
			if d.Ready && d.Operation == "" && d.Backup.Hours > 0 && !d.NextBackup.After(time.Now()) {
				if d.Backup.Offsite {
					if err := c.syncBackupStorage(request, result.Server.ID); err != nil {
						record(c.observe(ctx, "storage:"+result.Server.ID, "Off-site storage configuration could not reach "+result.Server.Name, true, 1))
						continue
					}
					record(c.observe(ctx, "storage:"+result.Server.ID, "Off-site storage configuration could not reach "+result.Server.Name, false, 1))
				}
				if err := c.agentCall(request, result.Server.ID, "POST", "/v1/databases/"+d.AppID+"/backup", nil, nil); err != nil {
					record(err)
				}
			}
		}
	}
	for _, result := range c.readFleet(request, "/v1/server-health") {
		if result.Err != nil {
			continue
		}
		var h agent.ServerHealth
		if json.Unmarshal(result.Body, &h) != nil {
			continue
		}
		for _, disk := range h.Disks {
			record(c.observe(ctx, "disk:"+result.Server.ID+":"+disk.Path, "Disk space is low on "+result.Server.Name+" ("+disk.Path+")", disk.UsedPercent >= 85 || disk.Available < 1<<30, 2))
		}
		if h.MemoryTotal > 0 {
			record(c.observe(ctx, "memory:"+result.Server.ID, "Memory is low on "+result.Server.Name, h.MemoryAvailable < h.MemoryTotal/10, 3))
		}
		if h.CPUs > 0 {
			record(c.observe(ctx, "load:"+result.Server.ID, "CPU load is high on "+result.Server.Name, h.Load1 > float64(h.CPUs)*2, 5))
		}
	}
	for _, result := range c.readFleet(request, "/v1/app-health") {
		if result.Err != nil {
			continue
		}
		var health []agent.AppHealth
		if json.Unmarshal(result.Body, &health) != nil {
			continue
		}
		for _, h := range health {
			if h.State != "healthy" && h.State != "unhealthy" {
				continue
			}
			record(c.observe(ctx, "health:"+result.Server.ID+":"+h.AppID, "Application "+h.AppID+" failed its readiness check", h.State == "unhealthy", 3))
		}
	}
	for _, result := range c.readFleet(request, "/v1/deployments") {
		if result.Err != nil {
			continue
		}
		var deployments []agent.Deployment
		if json.Unmarshal(result.Body, &deployments) != nil {
			continue
		}
		// Latest completed result per app clears a previous deployment incident.
		latest := map[string]agent.Deployment{}
		for _, d := range deployments {
			if d.Status != "failed" && d.Status != "live" {
				continue
			}
			if previous, ok := latest[d.AppID]; !ok || previous.Created.Before(d.Created) {
				latest[d.AppID] = d
			}
		}
		for id, d := range latest {
			if _, ok := c.app(id); !ok {
				continue
			}
			record(c.observe(ctx, "deploy:"+result.Server.ID+":"+id, "Deployment failed for "+id, d.Status == "failed", 1))
		}
	}
	record(c.tasksTick(ctx, request))
	return firstErr
}
