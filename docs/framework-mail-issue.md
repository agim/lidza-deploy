# Framework mail reload blocker

Tracked in [agim/lidza#27](https://github.com/agim/lidza/issues/27), opened against latest v0.1.70. Do not substitute a local mail implementation; upgrade only after a tagged fix and rerun the reproduction plus the database-backed operational tests.

Changing SMTP settings in a running app can race with mail.Send or a jobs worker delivering an outbox message. This blocks safe GUI mail configuration in agim/lidza-deploy. Our framework-first rule requires a released framework fix rather than an application substitute.

Reproduced on latest v0.1.70 with Go 1.27.1. Reconfigure assigns `m.cfg` at packs/mail/mail.go:198 without synchronizing readers; Send → prepare reads it at line 449. Queued Deliver also reads configuration at lines 386/399. The provider pointer lock does not protect this configuration. Templates/config/provider should be published as a consistent validated snapshot; concurrent Reconfigure calls also need synchronization. A failed reload should preserve the previous working snapshot.

Minimal reproduction (no database, network, or email delivery): save this as a test inside a module pinned to v0.1.70 and run `go test -race`.

```go
package mailrace
import("context";"io";"log/slog";"sync";"testing";"github.com/agim/lidza/packs/mail")
func TestConcurrentReconfigureSend(t *testing.T){
 t.Setenv("MAIL_PROVIDER","outbox");t.Setenv("MAIL_FROM","alerts@example.com")
 slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard,nil)))
 m,err:=mail.New(mail.Config{Provider:"outbox",From:"alerts@example.com"},nil,nil);if err!=nil{t.Fatal(err)}
 var wg sync.WaitGroup;wg.Add(2)
 go func(){defer wg.Done();for i:=0;i<200;i++{_ = m.Reconfigure(context.Background())}}()
 go func(){defer wg.Done();for i:=0;i<200;i++{_,_=m.Send(context.Background(),mail.Message{To:"operator@example.com",Subject:"fixture",Text:"fixture"})}}()
 wg.Wait()
}

```

Observed: `WARNING: DATA RACE`, Reconfigure mail.go:198 writing concurrently with prepare mail.go:449; test fails.

Requested acceptance checks:
- Concurrent Reconfigure, Send/SendTx, queued delivery, Provider and Link are race-free.
- Send/delivery observes a consistent config/provider/templates snapshot.
- Failed reload retains the previous valid sender/provider/templates.
- GUI SMTP settings can change while queued messages are delivered, without dropping or duplicating outbox work.
- Ship a tagged release and link it here so lidza-deploy can upgrade and resume its blocked email integration.
