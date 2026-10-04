package control

import (
	"bytes"
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/mail"
	"github.com/agim/lidza/packs/storage"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/agim/lidza/pkg/env"
	"net/http"
	netmail "net/mail"
	"strconv"
	"time"
)

func (c *Control) infrastructure(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	var store any
	if cfg := c.data.BackupStorage; cfg != nil {
		store = map[string]any{"endpoint": cfg.Endpoint, "bucket": cfg.Bucket, "region": cfg.Region, "prefix": cfg.Prefix, "configured": true}
	}
	incidents := []Incident{}
	for _, v := range c.data.Incidents {
		if v.Active {
			incidents = append(incidents, v)
		}
	}
	c.mu.Unlock()
	values, err := env.Values(".")
	if err != nil {
		agent.Fail(w, 500, errors.New("configuration unavailable"))
		return
	}
	messages := []map[string]any{}
	rows, err := db.From(r.Context()).Query(r.Context(), "SELECT id::text,subject,status,attempts,created_at FROM mail_message ORDER BY created_at DESC LIMIT 10")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, subject, status string
			var attempts int
			var created time.Time
			if rows.Scan(&id, &subject, &status, &attempts, &created) == nil {
				messages = append(messages, map[string]any{"id": id, "subject": subject, "status": status, "attempts": attempts, "created": created})
			}
		}
	}
	agent.JSON(w, 200, map[string]any{"storage": store, "mail": map[string]any{"configured": values["MAIL_PROVIDER"] == "smtp", "host": values["MAIL_SMTP_HOST"], "port": values["MAIL_SMTP_PORT"], "security": values["MAIL_SMTP_SECURITY"], "from": values["MAIL_FROM"], "recipient": c.cfg.User}, "incidents": incidents, "messages": messages})
}
func (c *Control) configureStorage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Endpoint string `json:"endpoint"`
		Bucket   string `json:"bucket"`
		Region   string `json:"region"`
		Prefix   string `json:"prefix"`
		Access   string `json:"access_key"`
		Secret   string `json:"secret_key"`
	}
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg := storage.Config{Provider: "s3", Endpoint: in.Endpoint, Bucket: in.Bucket, Region: in.Region, Prefix: in.Prefix, AccessKey: in.Access, SecretKey: in.Secret, Timeout: 15 * time.Second}
	if c.data.BackupStorage != nil && cfg.AccessKey == "" && cfg.SecretKey == "" {
		cfg.AccessKey = c.data.BackupStorage.AccessKey
		cfg.SecretKey = c.data.BackupStorage.SecretKey
	}
	if err := agent.ValidateBackupStorage(cfg); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	target, _ := storage.New(cfg)
	key := "connection-check/" + random()
	if _, err := target.Put(r.Context(), key, bytes.NewReader([]byte("Lidza Deploy storage check")), storage.PutOptions{ContentType: "text/plain"}); err != nil {
		agent.Fail(w, 400, errors.New("storage test failed; check endpoint, bucket and write permission"))
		return
	}
	if err := target.Delete(r.Context(), key); err != nil {
		agent.Fail(w, 400, errors.New("storage write succeeded but test cleanup failed; check delete permission"))
		return
	}
	old := c.data.BackupStorage
	c.data.BackupStorage = &cfg
	if err := c.save(); err != nil {
		c.data.BackupStorage = old
		agent.Fail(w, 500, errors.New("could not save encrypted storage configuration"))
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "verified"})
}
func (c *Control) configureMail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		Security string `json:"security"`
		From     string `json:"from"`
	}
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if _, err := netmail.ParseAddress(in.From); err != nil || in.Host == "" || in.Port < 1 || in.Port > 65535 || (in.Security != "tls" && in.Security != "starttls") {
		agent.Fail(w, 400, errors.New("enter a sender, SMTP host/port, and TLS or STARTTLS"))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	values, err := env.Values(".")
	if err != nil {
		agent.Fail(w, 500, errors.New("configuration unavailable"))
		return
	}
	if in.Username == "" && in.Password == "" {
		in.Username = values["MAIL_SMTP_USERNAME"]
		in.Password = values["MAIL_SMTP_PASSWORD"]
	}
	cfg := mail.Config{Provider: "smtp", From: in.From, SMTPHost: in.Host, SMTPPort: in.Port, SMTPUsername: in.Username, SMTPPassword: in.Password, SMTPSecurity: in.Security}
	if _, err = mail.New(cfg, nil, nil); err != nil {
		agent.Fail(w, 400, errors.New("invalid SMTP configuration"))
		return
	}
	patch := map[string]string{"MAIL_PROVIDER": "smtp", "MAIL_FROM": in.From, "MAIL_SMTP_HOST": in.Host, "MAIL_SMTP_PORT": strconv.Itoa(in.Port), "MAIL_SMTP_USERNAME": in.Username, "MAIL_SMTP_PASSWORD": in.Password, "MAIL_SMTP_SECURITY": in.Security, "MAIL_SMTP_URL": ""}
	origins, err := env.Origins(".")
	if err != nil {
		agent.Fail(w, 500, errors.New("configuration unavailable"))
		return
	}
	for k := range patch {
		if origins[k] == env.OriginProcess {
			agent.Fail(w, 409, errors.New("mail configuration has service environment overrides; remove them before using GUI settings"))
			return
		}
	}
	if err = credentials.Set(".", patch); err != nil {
		agent.Fail(w, 500, errors.New("could not save encrypted mail configuration"))
		return
	}
	if err = mail.From(r.Context()).Reconfigure(r.Context()); err != nil {
		agent.Fail(w, 500, errors.New("could not reload mail configuration"))
		return
	}
	agent.JSON(w, 200, map[string]string{"status": "saved"})
}
func (c *Control) testMail(w http.ResponseWriter, r *http.Request) {
	if !configuredMail() {
		agent.Fail(w, 409, errors.New("configure SMTP first"))
		return
	}
	id, err := mail.From(r.Context()).Send(r.Context(), mail.Message{To: c.cfg.User, Subject: "Līdza Deploy test alert", Text: "Email alerts are configured for your Līdza Deploy control panel."})
	if err != nil {
		agent.Fail(w, 502, errors.New("test email could not be queued"))
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "queued", "id": id})
}
func configuredMail() bool { v, e := env.Values("."); return e == nil && v["MAIL_PROVIDER"] == "smtp" }
