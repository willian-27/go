package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/patrickmn/go-cache"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type server struct {
	db     *sqlx.DB
	router *mux.Router
	exPath string
}

// Replace the global variables
var (
	address       = flag.String("address", "0.0.0.0", "Bind IP Address")
	port          = flag.String("port", "8080", "Listen Port")
	waDebug       = flag.String("wadebug", "", "Enable whatsmeow debug (INFO or DEBUG)")
	logType       = flag.String("logtype", "console", "Type of log output (console or json)")
	skipMedia     = flag.Bool("skipmedia", false, "Do not attempt to download media in messages")
	osName        = flag.String("osname", "Mac OS 10", "Connection OSName in Whatsapp")
	colorOutput   = flag.Bool("color", false, "Enable colored output for console logs")
	sslcert       = flag.String("sslcertificate", "", "SSL Certificate File")
	sslprivkey    = flag.String("sslprivatekey", "", "SSL Certificate Private Key File")
	adminToken    = flag.String("admintoken", "", "Security Token to authorize admin actions (list/create/remove users)")
	globalWebhook = flag.String("globalwebhook", "", "Global webhook URL to receive all events from all users")
	versionFlag   = flag.Bool("version", false, "Display version information and exit")

	container        *sqlstore.Container
	clientManager    = NewClientManager()
	killchannel      = make(map[string](chan bool))
	userinfocache    = cache.New(5*time.Minute, 10*time.Minute)
	lastMessageCache = cache.New(24*time.Hour, 24*time.Hour)
)

const version = "1.0.2"

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Warn().Err(err).Msg("It was not possible to load the .env file (it may not exist).")
	}

	flag.Parse()

	// Novo bloco para sobrescrever o osName pelo ENV, se existir
	if v := os.Getenv("SESSION_DEVICE_NAME"); v != "" {
		*osName = v
	}

	if *versionFlag {
		fmt.Printf("WuzAPI version %s\n", version)
		os.Exit(0)
	}
	tz := os.Getenv("TZ")
	if tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			log.Warn().Err(err).Msgf("It was not possible to define TZ=%q, using UTC", tz)
		} else {
			time.Local = loc
			log.Info().Str("TZ", tz).Msg("Timezone defined")
		}
	}

	if *logType == "json" {
		log.Logger = zerolog.New(os.Stdout).
			With().
			Timestamp().
			Str("role", filepath.Base(os.Args[0])).
			Logger()
	} else {
		output := zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "2006-01-02 15:04:05 -07:00",
			NoColor:    !*colorOutput,
		}

		output.FormatLevel = func(i interface{}) string {
			if i == nil {
				return ""
			}
			lvl := strings.ToUpper(i.(string))
			switch lvl {
			case "DEBUG":
				return "\x1b[34m" + lvl + "\x1b[0m"
			case "INFO":
				return "\x1b[32m" + lvl + "\x1b[0m"
			case "WARN":
				return "\x1b[33m" + lvl + "\x1b[0m"
			case "ERROR", "FATAL", "PANIC":
				return "\x1b[31m" + lvl + "\x1b[0m"
			default:
				return lvl
			}
		}

		log.Logger = zerolog.New(output).
			With().
			Timestamp().
			Str("role", filepath.Base(os.Args[0])).
			Logger()
	}

	if *adminToken == "" {
		if v := os.Getenv("WUZAPI_ADMIN_TOKEN"); v != "" {
			*adminToken = v
		} else {
			// Generate a random token if none provided
			const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
			b := make([]byte, 32)
			for i := range b {
				b[i] = charset[rand.Intn(len(charset))]
			}
			*adminToken = string(b)
			log.Warn().Str("admin_token", *adminToken).Msg("No admin token provided, generated a random one")
		}
	}

	// Check for global webhook in environment variable
	if *globalWebhook == "" {
		if v := os.Getenv("WUZAPI_GLOBAL_WEBHOOK"); v != "" {
			*globalWebhook = v
			log.Info().Str("global_webhook", v).Msg("Global webhook configured from environment variable")
		}
	} else {
		log.Info().Str("global_webhook", *globalWebhook).Msg("Global webhook configured from command line")
	}

	InitRabbitMQ()
}

func main() {
	ex, err := os.Executable()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to get executable path")
		panic(err)
	}
	exPath := filepath.Dir(ex)

	db, err := InitializeDatabase(exPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize database")
		os.Exit(1)
	}
	// Defer cleanup of the database connection
	defer func() {
		if err := db.Close(); err != nil {
			log.Error().Err(err).Msg("Failed to close database connection")
		}
	}()

	// Initialize the schema
	if err = initializeSchema(db); err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize schema")
		// Perform cleanup before exiting
		if err := db.Close(); err != nil {
			log.Error().Err(err).Msg("Failed to close database connection during cleanup")
		}
		os.Exit(1)
	}

	var dbLog waLog.Logger
	if *waDebug != "" {
		dbLog = waLog.Stdout("Database", *waDebug, *colorOutput)
	}

	// Get database configuration
	config := getDatabaseConfig(exPath)
	var storeConnStr string
	if config.Type == "postgres" {
		storeConnStr = fmt.Sprintf(
			"user=%s password=%s dbname=%s host=%s port=%s sslmode=%s",
			config.User, config.Password, config.Name, config.Host, config.Port, config.SSLMode,
		)
		container, err = sqlstore.New(context.Background(), "postgres", storeConnStr, dbLog)
	} else {
		storeConnStr = "file:" + filepath.Join(config.Path, "main.db") + "?_pragma=foreign_keys(1)&_busy_timeout=3000"
		container, err = sqlstore.New(context.Background(), "sqlite", storeConnStr, dbLog)
	}

	if err != nil {
		log.Fatal().Err(err).Msg("Error creating sqlstore")
		os.Exit(1)
	}

	s := &server{
		router: mux.NewRouter(),
		db:     db,
		exPath: exPath,
	}
	s.routes()

	s.connectOnStartup()

	srv := &http.Server{
		Addr:              *address + ":" + *port,
		Handler:           s.router,
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       180 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	var once sync.Once

	// Wait for signals in a separate goroutine
	go func() {
		for {
			<-done
			once.Do(func() {
				log.Warn().Msg("Stopping server...")

				// Graceful shutdown logic
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				if err := srv.Shutdown(ctx); err != nil {
					log.Error().Err(err).Msg("Failed to stop server")
					os.Exit(1)
				}

				log.Info().Msg("Server Exited Properly")
				os.Exit(0)
			})
		}
	}()

	go func() {
		if *sslcert != "" {

			if *sslcert != "" && *sslprivkey != "" {
				if _, err := os.Stat(*sslcert); os.IsNotExist(err) {
					log.Fatal().Err(err).Msg("SSL certificate file does not exist")
				}
				if _, err := os.Stat(*sslprivkey); os.IsNotExist(err) {
					log.Fatal().Err(err).Msg("SSL private key file does not exist")
				}
			}
			if err := srv.ListenAndServeTLS(*sslcert, *sslprivkey); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("HTTPS server failed to start")
			}
		} else {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("HTTP server failed to start")
			}
		}
	}()
	log.Info().Str("address", *address).Str("port", *port).Msg("Server started. Waiting for connections...")
	select {}

}
