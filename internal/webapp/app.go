package webapp

import (
	"context"
	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/control"
	"github.com/agim/lidza-deploy/web"
	"github.com/agim/lidza/packs/audit"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/jobs"
	"github.com/agim/lidza/packs/mail"
	"github.com/agim/lidza/pkg/router"
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
	return lidza.Boot(ctx, lidza.App{Name: "lidza-deploy", Frontend: h, Packs: []lidza.Pack{db.Pack(), auth.Pack(), audit.Pack(), jobs.Pack(), mail.Pack()}, OnStart: c.Start, Routes: func(r *router.Router) {
		auth.Mount(r, auth.Options{ConnectAuthorize: c.AuthorizeConnect, NoRegister: true, Providers: []auth.Provider{}, AfterSignIn: "/console.html", Title: "Līdza Deploy"})
		r.Handle("/api/control/", h)
	}})
}
