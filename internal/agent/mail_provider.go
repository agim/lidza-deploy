package agent

import (
	"net"
	"net/url"
	"slices"
	"strings"
)

// OutgoingMailProvider names the provider an application sends its mail
// through, from its environment: a provider's API settings (MAILGUN_*,
// SENDGRID_*, POSTMARK_*, AWS SES, RESEND_*, ...), MAIL_PROVIDER, or an
// SMTP server on another host. "" when the environment names none, so the
// application may deliver mail itself from the server's address. The move
// plan uses it: reverse DNS of the destination matters only for mail the
// server delivers itself.
func OutgoingMailProvider(env map[string]string) string {
	set := func(key string) bool { return strings.TrimSpace(env[key]) != "" }
	keys := make([]string, 0, len(env))
	for key := range env {
		if set(key) {
			keys = append(keys, strings.ToUpper(key))
		}
	}
	slices.Sort(keys)
	prefixes := []struct{ prefix, name string }{
		{"MAILGUN_", "Mailgun"}, {"SENDGRID_", "SendGrid"}, {"POSTMARK_", "Postmark"}, {"RESEND_", "Resend"},
		{"AWS_SES_", "Amazon SES"}, {"SES_", "Amazon SES"}, {"MAILJET_", "Mailjet"}, {"SPARKPOST_", "SparkPost"},
		{"BREVO_", "Brevo"}, {"SENDINBLUE_", "Brevo"}, {"MANDRILL_", "Mandrill"},
	}
	for _, p := range prefixes {
		for _, key := range keys {
			if strings.HasPrefix(key, p.prefix) || strings.Contains(key, "_"+p.prefix) {
				return p.name
			}
		}
	}
	value := func(names ...string) string {
		for _, name := range names {
			for key, v := range env {
				if strings.EqualFold(key, name) && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
		return ""
	}
	host := value("SMTP_ADDRESS", "SMTP_HOST", "SMTP_SERVER", "MAIL_SMTP_HOST", "MAIL_SMTP_ADDRESS", "MAIL_HOST", "EMAIL_HOST", "SMTP_HOSTNAME")
	if raw := value("SMTP_URL", "MAIL_SMTP_URL", "MAILER_URL", "EMAIL_URL"); host == "" && raw != "" {
		if u, err := url.Parse(raw); err == nil && strings.HasPrefix(strings.ToLower(u.Scheme), "smtp") {
			host = u.Hostname()
		}
	}
	remote := remoteMailHost(host)
	switch provider := strings.ToLower(value("MAIL_PROVIDER", "MAILER_PROVIDER", "EMAIL_PROVIDER", "MAIL_DELIVERY_METHOD")); provider {
	case "mailgun", "sendgrid", "postmark", "resend", "ses", "amazon_ses", "aws_ses", "mailjet", "sparkpost", "brevo":
		return map[string]string{"mailgun": "Mailgun", "sendgrid": "SendGrid", "postmark": "Postmark", "resend": "Resend", "ses": "Amazon SES", "amazon_ses": "Amazon SES",
			"aws_ses": "Amazon SES", "mailjet": "Mailjet", "sparkpost": "SparkPost", "brevo": "Brevo"}[provider]
	}
	if remote == "" {
		return ""
	}
	known := []struct{ suffix, name string }{
		{"mailgun.org", "Mailgun"}, {"mailgun.net", "Mailgun"}, {"sendgrid.net", "SendGrid"}, {"postmarkapp.com", "Postmark"},
		{"amazonaws.com", "Amazon SES"}, {"resend.com", "Resend"}, {"mailjet.com", "Mailjet"}, {"sparkpostmail.com", "SparkPost"},
		{"brevo.com", "Brevo"}, {"sendinblue.com", "Brevo"}, {"gmail.com", "Google"}, {"googlemail.com", "Google"},
		{"office365.com", "Microsoft 365"}, {"outlook.com", "Microsoft 365"}, {"mandrillapp.com", "Mandrill"}, {"zoho.com", "Zoho"}, {"zoho.eu", "Zoho"},
	}
	for _, k := range known {
		if remote == k.suffix || strings.HasSuffix(remote, "."+k.suffix) {
			return k.name
		}
	}
	return "the SMTP server " + remote
}

// remoteMailHost returns host when it is a mail server on another machine:
// a public address, or a qualified name that is not loopback. Single-label
// names (a container or a local alias) and private addresses are local.
func remoteMailHost(host string) string {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]."))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			return ""
		}
		return host
	}
	if !strings.Contains(host, ".") {
		return ""
	}
	return host
}
