package webapp

import (
	"context"
	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/control"
	"github.com/agim/lidza-deploy/web"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/jobs"
	"github.com/agim/lidza/packs/mail"
	"github.com/agim/lidza/pkg/middleware"
	"github.com/agim/lidza/pkg/router"
	"strings"
	"time"
)

func Boot(ctx context.Context) (*lidza.Booted, error) {
	cfg, err := control.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	c, err := control.New(cfg)
	if err != nil {
		return nil, err
	}
	h := c.Handler(web.Handler())
	// The framework context must outlive the console's command and response deadlines.
	// HTTP write deadlines still bound ordinary requests; only the console extends its own.
	return lidza.Boot(ctx, lidza.App{Name: "lidza-deploy", Timeout: agent.ConsoleResponseTimeout + 15*time.Second, CSP: strings.Replace(middleware.DefaultCSP, "form-action 'self'", "form-action 'self' https://github.com", 1), Frontend: h, Packs: []lidza.Pack{db.Pack(), auth.Pack(), audit.Pack(), jobs.Pack(), mail.Pack()}, OnStart: c.Start, Routes: func(r *router.Router) {
		auth.Mount(r, auth.Options{ConnectAuthorize: c.AuthorizeConnect, NoRegister: true, Providers: []auth.Provider{}, AfterSignIn: "/console.html", Title: "Līdza Deploy", TwoFactorIssuer: "Līdza Deploy"})
		r.Handle("/api/control/", h)
		r.Handle("/api/agent/", h)
	}})
}
