package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/FedericoArdesi/mcsync/internal/api"
	"github.com/FedericoArdesi/mcsync/internal/client"
)

// handoffAPI è la parte del servizio che serve a chiedere il passaggio di host.
type handoffAPI interface {
	Status(ctx context.Context) (api.Status, error)
	RequestHandoff(ctx context.Context) (api.HandoffRequest, error)
	CancelHandoff(ctx context.Context) error
}

var _ handoffAPI = (*client.Client)(nil)

const handoffCallTimeout = 15 * time.Second

// waitForHandoff chiede all'host di passare la mano e aspetta che lo faccia
// (6.7). Restituisce nil quando si può provare a prendere la lease: è
// riservata a chi chiama, oppure nessuno gioca più. Se non si arriva a quel
// punto (timeout, Ctrl+C) la richiesta viene ritirata, così l'host non resta
// con una richiesta che nessuno aspetta e non riserva la lease a vuoto.
func waitForHandoff(ctx context.Context, cl handoffAPI, timeout, poll time.Duration, out io.Writer) error {
	st, err := statusCall(ctx, cl)
	if err != nil {
		return fmt.Errorf("il laptop non risponde: %w", err)
	}
	me := st.You.Player
	switch {
	case st.Reservation != nil && st.Reservation.For == me:
		fmt.Fprintln(out, "La lease è già riservata a te: parto subito.")
		return nil
	case st.Reservation != nil:
		return fmt.Errorf("la lease è riservata a %s per un passaggio di host (ancora %d s): riprova tra poco", st.Reservation.For, st.Reservation.ExpiresInSeconds)
	case st.Lease == nil:
		fmt.Fprintln(out, "Nessuno sta giocando: non serve chiedere il passaggio.")
		return nil
	case st.Lease.Player == me:
		return errors.New("sei già host (forse da un altro dispositivo): non serve chiedere il passaggio")
	}
	host := st.Lease.Player

	rctx, cancel := context.WithTimeout(ctx, handoffCallTimeout)
	_, err = cl.RequestHandoff(rctx)
	cancel()
	switch {
	case client.IsCode(err, api.CodeHandoffPending):
		return errors.New("c'è già una richiesta di passaggio in corso: non se ne può fare un'altra, riprova tra poco")
	case client.IsCode(err, api.CodeNoLease, api.CodeLeaseExpired):
		fmt.Fprintln(out, "L'host non c'è più: provo a prendere io la lease.")
		return nil
	case err != nil:
		return fmt.Errorf("richiesta di passaggio: %w", err)
	}
	fmt.Fprintf(out, "Ho chiesto a %s di passarti la mano: salva il mondo e ti avvisa (Ctrl+C per ritirare la richiesta).\n", host)

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			withdrawHandoff(ctx, cl, out)
			return fmt.Errorf("attesa interrotta: %w", ctx.Err())
		case <-deadline.C:
			withdrawHandoff(ctx, cl, out)
			return fmt.Errorf("%s non ha risposto entro %s: richiesta ritirata", host, timeout)
		case <-tick.C:
		}
		st, err := statusCall(ctx, cl)
		if err != nil {
			if ctx.Err() != nil {
				continue // lo gestisce il ramo ctx.Done
			}
			fmt.Fprintf(out, "Il laptop non risponde (%v): riprovo.\n", err)
			continue
		}
		switch {
		case st.Reservation != nil && st.Reservation.For == me:
			fmt.Fprintf(out, "%s ha salvato e ti ha passato la mano: avvio.\n", host)
			return nil
		case st.Reservation != nil:
			return fmt.Errorf("la lease è stata riservata a %s: la tua richiesta non c'è più", st.Reservation.For)
		case st.Lease == nil:
			fmt.Fprintf(out, "%s ha smesso di giocare: provo a prendere la lease.\n", host)
			return nil
		case st.Handoff == nil || st.Handoff.To != me:
			return errors.New("la tua richiesta di passaggio non c'è più (l'host è cambiato?): riprova")
		}
	}
}

func statusCall(ctx context.Context, cl handoffAPI) (api.Status, error) {
	cctx, cancel := context.WithTimeout(ctx, handoffCallTimeout)
	defer cancel()
	return cl.Status(cctx)
}

// withdrawHandoff ritira la richiesta con un contesto nuovo: quando si arriva
// qui spesso il contesto di chi chiama è già annullato (Ctrl+C), e il ritiro
// non partirebbe mai.
func withdrawHandoff(ctx context.Context, cl handoffAPI, out io.Writer) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handoffCallTimeout)
	defer cancel()
	switch err := cl.CancelHandoff(wctx); {
	case err == nil:
		fmt.Fprintln(out, "Richiesta ritirata.")
	case client.IsCode(err, api.CodeNoHandoff):
	// già sparita (l'host aveva appena finito): niente da ritirare
	default:
		fmt.Fprintf(out, "Non sono riuscito a ritirare la richiesta (%v): scadrà con la lease dell'host.\n", err)
	}
}
