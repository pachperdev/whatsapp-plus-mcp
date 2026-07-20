package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
)

// fakeAppStateClient simula las respuestas del cliente whatsmeow para sendAppState:
// consume las colas de errores en orden (vacía = nil) y registra cada llamada, para
// poder afirmar el NÚMERO de intentos y el flag fullSync de cada fetch.
type fakeAppStateClient struct {
	sendErrs  []error // respuestas sucesivas de SendAppState (agotada = nil)
	fetchErrs []error // respuestas sucesivas de FetchAppState (agotada = nil)
	peerErr   error   // respuesta fija de SendPeerMessage

	sendCalls     int
	fetchCalls    int
	peerCalls     int
	fetchFullSync []bool // flag fullSync de cada FetchAppState, en orden
}

func (f *fakeAppStateClient) SendAppState(ctx context.Context, patch appstate.PatchInfo) error {
	f.sendCalls++
	if len(f.sendErrs) == 0 {
		return nil
	}
	err := f.sendErrs[0]
	f.sendErrs = f.sendErrs[1:]
	return err
}

func (f *fakeAppStateClient) FetchAppState(ctx context.Context, name appstate.WAPatchName, fullSync, onlyIfNotSynced bool) error {
	f.fetchCalls++
	f.fetchFullSync = append(f.fetchFullSync, fullSync)
	if len(f.fetchErrs) == 0 {
		return nil
	}
	err := f.fetchErrs[0]
	f.fetchErrs = f.fetchErrs[1:]
	return err
}

func (f *fakeAppStateClient) SendPeerMessage(ctx context.Context, message *waE2E.Message) (whatsmeow.SendResponse, error) {
	f.peerCalls++
	return whatsmeow.SendResponse{}, f.peerErr
}

// TestSendAppState cubre la recuperación en dos niveles ante un conflicto LTHash,
// que antes del seam appStateSender estaba al 0% de cobertura (imposible provocar
// un 409 real en tests). Los mensajes de error afirmados son el contrato actual.
func TestSendAppState(t *testing.T) {
	// Sin esto, los casos que entran al poll de recuperación dormirían 3s por
	// iteración (hasta 18s por caso). El intervalo real no cambia en producción.
	orig := appStateRecoveryPollInterval
	appStateRecoveryPollInterval = time.Millisecond
	t.Cleanup(func() { appStateRecoveryPollInterval = orig })

	conflict := errors.New(`server returned error updating app state (regular_high): <error code="409" text="conflict"/> (mismatching LTHash)`)
	fetchFail := errors.New("failed to decode app state regular_high patches: mismatching LTHash")
	otherErr := errors.New("websocket disconnected")
	peerErr := errors.New("peer message rejected")
	patch := appstate.PatchInfo{Type: appstate.WAPatchRegularHigh}

	// repeat arma una cola de n errores idénticos (para agotar el poll de 6 intentos).
	repeat := func(err error, n int) []error {
		out := make([]error, n)
		for i := range out {
			out[i] = err
		}
		return out
	}

	tests := []struct {
		name          string
		fake          *fakeAppStateClient
		wantErr       string // "" = éxito; si no, mensaje EXACTO del error devuelto
		wantSend      int
		wantFetch     int
		wantPeer      int
		wantFullSyncs []bool // flag fullSync esperado de cada fetch, en orden
	}{
		{
			name:     "éxito directo: sin conflicto no hay resync ni recovery",
			fake:     &fakeAppStateClient{},
			wantSend: 1,
		},
		{
			name:     "error ajeno se devuelve tal cual, sin reintentos",
			fake:     &fakeAppStateClient{sendErrs: []error{otherErr}},
			wantErr:  otherErr.Error(),
			wantSend: 1,
		},
		{
			name: "conflicto LTHash: resync completo y reintento exitoso",
			fake: &fakeAppStateClient{
				sendErrs: []error{conflict}, // el reintento (cola vacía) devuelve nil
			},
			wantSend:      2,
			wantFetch:     1,
			wantFullSyncs: []bool{true}, // el resync de nivel 1 debe ser fullSync
		},
		{
			name: "conflicto persistente: recovery vía teléfono primario con poll",
			fake: &fakeAppStateClient{
				sendErrs: []error{conflict},
				// fetch 1 (fullSync) corrupto; fetch 2 aún sin copia; fetch 3 ya llegó.
				fetchErrs: []error{fetchFail, errors.New("todavía no llegó la copia")},
			},
			wantSend:      2,
			wantFetch:     3,
			wantPeer:      1,
			wantFullSyncs: []bool{true, false, false}, // el poll NO re-borra el estado local
		},
		{
			name: "fallo total: el poll se agota y devuelve el error acotado",
			fake: &fakeAppStateClient{
				// 1 conflicto inicial + 6 en el poll (uno por iteración).
				sendErrs:  repeat(conflict, 7),
				fetchErrs: []error{fetchFail}, // el resto de fetches (poll) devuelven nil
			},
			wantErr:   fmt.Sprintf("app state %s en recuperación: se pidió una copia limpia al teléfono primario (debe estar online); reintentá la operación en unos segundos (error original: %v)", patch.Type, conflict),
			wantSend:  7,
			wantFetch: 7,
			wantPeer:  1,
		},
		{
			name: "resync corrupto y recovery inalcanzable: error con ambas causas",
			fake: &fakeAppStateClient{
				sendErrs:  []error{conflict},
				fetchErrs: []error{fetchFail},
				peerErr:   peerErr,
			},
			wantErr:   fmt.Sprintf("resync de app state %s fallido (%v) y no se pudo pedir recovery al teléfono primario: %v", patch.Type, fetchFail, peerErr),
			wantSend:  1,
			wantFetch: 1,
			wantPeer:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sendAppState(tc.fake, patch)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("esperaba éxito, got %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("esperaba error %q, got nil", tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Errorf("error:\n got  %q\n want %q", err.Error(), tc.wantErr)
				}
			}
			if tc.fake.sendCalls != tc.wantSend {
				t.Errorf("SendAppState calls: got %d, want %d", tc.fake.sendCalls, tc.wantSend)
			}
			if tc.fake.fetchCalls != tc.wantFetch {
				t.Errorf("FetchAppState calls: got %d, want %d", tc.fake.fetchCalls, tc.wantFetch)
			}
			if tc.fake.peerCalls != tc.wantPeer {
				t.Errorf("SendPeerMessage calls: got %d, want %d", tc.fake.peerCalls, tc.wantPeer)
			}
			if tc.wantFullSyncs != nil {
				if len(tc.fake.fetchFullSync) != len(tc.wantFullSyncs) {
					t.Fatalf("fetchFullSync: got %v, want %v", tc.fake.fetchFullSync, tc.wantFullSyncs)
				}
				for i, want := range tc.wantFullSyncs {
					if tc.fake.fetchFullSync[i] != want {
						t.Errorf("fetch #%d fullSync: got %v, want %v", i+1, tc.fake.fetchFullSync[i], want)
					}
				}
			}
		})
	}
}
