package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/mdp/qrterminal"
	_ "modernc.org/sqlite"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"whatsapp-client/internal/api"
	"whatsapp-client/internal/auth"
	"whatsapp-client/internal/config"
	"whatsapp-client/internal/store"
	"whatsapp-client/internal/wa"
)

// goSafe corre fn en una goroutine con recover. Los handlers de eventos que corren
// en goroutine (handleHistorySync, handlePollVote, handleCallOffer) procesan protobufs
// influidos por la red y viven FUERA del recover per-request de net/http: un panic ahi
// tumbaria todo el proceso. Aca lo logueamos con stack y seguimos vivos.
func goSafe(logger waLog.Logger, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("panic recuperado en %s: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Config resuelta una sola vez (env con defaults = layout historico). Todos los
	// paths quedan absolutos: el bridge deja de depender del CWD del proceso.
	cfg, err := config.Load()
	if err != nil {
		logger.Errorf("Config invalida: %v", err)
		return
	}

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(cfg.StoreDir, 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	// ToSlash: el DSN file: usa '/' en todas las plataformas.
	sessionDBPath := filepath.ToSlash(filepath.Join(cfg.StoreDir, "whatsapp.db"))
	container, err := sqlstore.New(context.Background(), "sqlite", "file:"+sessionDBPath+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance with extended timeout settings
	clientLog := waLog.Stdout("Client/Socket", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Enable automatic reconnection
	client.EnableAutoReconnect = true
	client.EmitAppStateEventsOnFullSync = false

	// Initialize message store
	messageStore, err := store.NewMessageStore(cfg.StoreDir)
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer func() { _ = messageStore.Close() }()

	// Validador anti-exfiltracion de rutas de media (conoce el store para protegerlo).
	validator := auth.NewValidator(cfg.StoreDir)

	// Servicio con la lógica stateful de WhatsApp (estado en memoria inyectado, no global).
	svc := wa.NewService(client, messageStore, logger, cfg.StoreDir, validator)

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Voto de encuesta entrante: descifrar y registrar (goroutine; no bloquea el dispatch).
			if v.Message.GetPollUpdateMessage() != nil {
				goSafe(logger, "handlePollVote", func() { svc.HandlePollVote(v) })
			}
			// Process regular messages
			svc.HandleMessage(v)

		case *events.HistorySync:
			// Procesar en goroutine para NO bloquear el dispatch de eventos en vivo.
			// El history-sync puede tardar minutos (cientos de mensajes + lookups de red);
			// si corre sincronico en el handler, los *events.Message en vivo quedan
			// encolados detras y no se guardan hasta que termina (bug observado).
			goSafe(logger, "handleHistorySync", func() { svc.HandleHistorySync(v) })

		case *events.Receipt:
			// T3-3: read-receipt PROPIO (leí el chat desde el teléfono u otro device) ->
			// marcar ese chat como leído. (ReceiptTypeRead = otros leyeron MIS mensajes, no aplica.)
			if v.Type == types.ReceiptTypeReadSelf {
				if n, _ := messageStore.ClearChatUnread(v.Chat.String()); n > 0 {
					logger.Infof("Chat %s marcado como leído (read-self): %d no-leídos limpiados", v.Chat.String(), n)
				}
			}

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			svc.OnConnected()

		case *events.Disconnected:
			// EnableAutoReconnect (mas arriba) ya gestiona la reconexion. Un reconnect
			// manual aqui competiria con el interno de whatsmeow (race / StreamReplaced).
			logger.Warnf("Disconnected from WhatsApp; auto-reconnect en curso...")
			svc.OnDisconnected()

		case *events.LoggedOut:
			logger.Warnf("Device logged out (reason=%s, onConnect=%v); please scan QR code to log in again", v.Reason.String(), v.OnConnect)
			svc.OnLoggedOut(v.Reason.String())

		case *events.TemporaryBan:
			// 🔴 Ban temporal: loggear fuerte y pausar envios (svc.SendMessage chequea isTempBanned).
			logger.Errorf("⚠️ TEMPORARY BAN: code=%d (%s); expira en %s. ENVIOS PAUSADOS.", int(v.Code), v.Code.String(), v.Expire)
			svc.OnTempBan(int(v.Code), v.Code.String(), v.Expire)

		case *events.ConnectFailure:
			logger.Errorf("Connect failure: reason=%d (%s) msg=%s", int(v.Reason), v.Reason.String(), v.Message)
			svc.OnConnectFailure(fmt.Sprintf("%d %s: %s", int(v.Reason), v.Reason.String(), v.Message))
			if v.Reason.IsLoggedOut() {
				svc.OnLoggedOut(v.Reason.String())
			}

		case *events.Presence:
			// Presencia de terceros: online/offline + last-seen (requiere SubscribePresence + SendPresence).
			svc.OnPresenceEvent(v.From, v.Unavailable, v.LastSeen)

		case *events.ChatPresence:
			// Typing de terceros (composing/paused) en un chat.
			svc.OnChatPresenceEvent(v.Sender, v.State == types.ChatPresenceComposing)

		case *events.CallOffer:
			// Llamada entrante: solo se registra (whatsmeow no maneja audio). Sin auto-rechazo.
			goSafe(logger, "handleCallOffer", func() { svc.HandleCallOffer(v) })
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				// Code crudo para poder generar un PNG nitido fuera de la terminal (el ASCII
				// half-block renderiza muy lento en algunos clientes y el QR expira antes).
				fmt.Printf("QR_RAW>>>%s<<<\n", evt.Code)
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server
	token, tokErr := auth.GetOrCreateBridgeToken(cfg.StoreDir)
	if tokErr != nil {
		fmt.Printf("WARNING: could not set up auth token: %v\n", tokErr)
	}
	handler := api.NewServer(svc, client, messageStore, token)

	// Bind SOLO a loopback (config.Load ya valido que cfg.Addr no se exponga a la red)
	// + timeouts (anti cliente lento/DoS).
	serverAddr := cfg.Addr
	srv := &http.Server{
		Addr:         serverAddr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block. ErrServerClosed es el retorno
	// NORMAL de un apagado ordenado (srv.Shutdown), no un error: no lo logueamos.
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Apagado ordenado del server HTTP: drena las requests en vuelo (con un tope de
	// 5s) ANTES de desconectar el cliente, para que ninguna toque una DB/cliente
	// cerrándose. El defer messageStore.Close() corre al final del main.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		fmt.Printf("REST server shutdown error: %v\n", err)
	}
	// Disconnect client
	client.Disconnect()
}
