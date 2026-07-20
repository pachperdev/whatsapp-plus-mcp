package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mdp/qrterminal"
	_ "modernc.org/sqlite"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"whatsapp-client/internal/api"
	"whatsapp-client/internal/auth"
	"whatsapp-client/internal/config"
	"whatsapp-client/internal/store"
	"whatsapp-client/internal/wa"
)

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

	// Validador anti-exfiltracion de rutas de media: protege el store y, si se
	// configuro WHATSAPP_MEDIA_ALLOWED_DIRS, confina los envios a esos directorios.
	validator := auth.NewValidator(cfg.StoreDir, cfg.MediaAllowedDirs)

	// Servicio con la lógica stateful de WhatsApp (estado en memoria inyectado, no global).
	svc := wa.NewService(client, messageStore, logger, cfg.StoreDir, validator)

	// El dispatcher de eventos es lógica de dominio: vive en wa (Service.HandleEvent),
	// junto a los handlers que invoca. main solo lo registra.
	client.AddEventHandler(svc.HandleEvent)

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// El server HTTP arranca ANTES del pairing: durante el modo QR el supervisor MCP
	// necesita consultar /api/qr y /api/status (el resto de rutas responde error hasta
	// que haya sesión). Si el server arrancara después, el QR solo existiría en stdout.
	token, tokErr := auth.GetOrCreateBridgeToken(cfg.StoreDir)
	if tokErr != nil {
		fmt.Printf("WARNING: could not set up auth token: %v\n", tokErr)
	}

	// shutdownRequested permite a /api/shutdown reciclar el proceso por el mismo camino
	// ordenado que SIGINT/SIGTERM (el supervisor lo usa para renovar sesiones zombie).
	shutdownRequested := make(chan struct{}, 1)
	requestShutdown := func() {
		select {
		case shutdownRequested <- struct{}{}:
		default:
		}
	}
	handler := api.NewServer(svc, client, messageStore, token, requestShutdown)

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

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Publicar cada código en el estado (para /api/qr) además de imprimirlo.
		for evt := range qrChan {
			if evt.Event == "code" {
				svc.SetQRCode(evt.Code, evt.Timeout)
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				// Code crudo para poder generar un PNG nitido fuera de la terminal (el ASCII
				// half-block renderiza muy lento en algunos clientes y el QR expira antes).
				fmt.Printf("QR_RAW>>>%s<<<\n", evt.Code)
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				svc.SetQRStatus("success")
				connected <- true
				break
			} else if evt.Event == "timeout" {
				// Canal QR agotado sin escaneo: dejar el estado visible en /api/qr y salir;
				// el supervisor recicla el proceso para obtener un canal fresco.
				svc.SetQRStatus("timeout")
				logger.Errorf("Timeout waiting for QR code scan")
				return
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			svc.SetQRStatus("timeout")
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

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal (SIGINT/SIGTERM) o apagado pedido via /api/shutdown
	select {
	case <-exitChan:
	case <-shutdownRequested:
		fmt.Println("Shutdown requested via /api/shutdown")
	}

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
