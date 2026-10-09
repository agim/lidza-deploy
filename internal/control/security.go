package control

import (
	"context"
	"github.com/agim/lidza/packs/db"
)

func (c *Control) securityTick(ctx context.Context) error {
	c.mu.Lock()
	apps := make([]Application, 0, len(c.data.Apps))
	for _, a := range c.data.Apps {
		if !a.Retiring {
			apps = append(apps, a)
		}
	}
	c.mu.Unlock()
	for _, a := range apps {
		var burst, review int
		err := db.From(ctx).QueryRow(ctx, `SELECT count(*) FILTER (WHERE created_at>now()-interval '5 minutes'),count(*) FILTER (WHERE extra->'security'->>'outcome'='review-response' AND created_at>now()-interval '1 hour') FROM app_error WHERE extra->>'deploy_server'=$1 AND extra->>'deploy_app'=$2 AND extra->>'deploy_generation'=$3 AND extra->'security' IS NOT NULL AND extra->'security'!='null'::jsonb AND created_at>now()-interval '1 hour'`, a.ServerID, a.ID, a.Generation).Scan(&burst, &review)
		if err != nil {
			return err
		}
		if err = c.observe(ctx, "security-burst:"+a.ServerID+":"+a.ID, "Probe burst detected for "+a.ID+" (50 or more captured requests in five minutes)", burst >= 50, 1); err != nil {
			return err
		}
		if err = c.observe(ctx, "security-review:"+a.ServerID+":"+a.ID, "Suspicious probe received a successful response for "+a.ID+"; review its response, since a fallback page can also return 200", review > 0, 1); err != nil {
			return err
		}
	}
	return nil
}
