package main

import (
	"context"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"runtime/debug"

	"contrib.go.opencensus.io/exporter/stackdriver"
	"github.com/99designs/gqlgen/handler"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/icco/graphql"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	"github.com/qor/auth"
	"github.com/qor/auth/auth_identity"
	"github.com/qor/auth/providers/password"
	qr "github.com/qor/render"
	"github.com/qor/session/manager"
	"github.com/rs/cors"
	"go.opencensus.io/exporter/prometheus"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/trace"
	"gopkg.in/unrolled/render.v1"
	"gopkg.in/unrolled/secure.v1"
)

var (
	// Renderer is a renderer for all occasions. These are our preferred default options.
	// See:
	//  - https://github.com/unrolled/render/blob/v1/README.md
	//  - https://godoc.org/gopkg.in/unrolled/render.v1
	Renderer = render.New(render.Options{
		Charset:                   "UTF-8",
		Directory:                 "./server/views",
		DisableHTTPErrorRendering: false,
		Extensions:                []string{".tmpl", ".html"},
		IndentJSON:                false,
		IndentXML:                 true,
		Layout:                    "layout",
		RequirePartials:           true,
		Funcs:                     []template.FuncMap{template.FuncMap{}},
	})

	dbUrl     = os.Getenv("DATABASE_URL")
	gormDB, _ = gorm.Open("postgres", dbUrl)

	// Auth contains auth config for middleware
	Auth = auth.New(&auth.Config{
		DB: gormDB,
		Render: qr.New(&qr.Config{
			FuncMapMaker: func(r *qr.Render, req *http.Request, w http.ResponseWriter) template.FuncMap {
				return template.FuncMap{
					"t": func(key string, args ...interface{}) template.HTML {
						// TODO: pull in from some sort of i18n thing
						return template.HTML(key)
					},
				}
			},
			ViewPaths: []string{
				"./auth_views",
				"./server/auth_views",
				"/go/src/github.com/icco/graphql/server/auth_views",
			},
		}),
	})
)

func init() {
	gormDB.AutoMigrate(&auth_identity.AuthIdentity{})
	Auth.RegisterProvider(password.New(&password.Config{}))
}

func main() {
	if dbUrl == "" {
		log.Panicf("DATABASE_URL is empty!")
	}
	log.Printf("Got DB URL %s", dbUrl)

	graphql.InitDB(dbUrl)

	port := "8080"
	if fromEnv := os.Getenv("PORT"); fromEnv != "" {
		port = fromEnv
	}
	log.Printf("Starting up on http://localhost:%s", port)

	pe, err := prometheus.NewExporter(prometheus.Options{
		Namespace: "graphql",
	})
	if err != nil {
		log.Fatalf("Failed to create the Prometheus exporter: %v", err)
	}
	view.RegisterExporter(pe)

	if os.Getenv("ENABLE_STACKDRIVER") != "" {
		sd, err := stackdriver.NewExporter(stackdriver.Options{
			ProjectID:    "icco-cloud",
			MetricPrefix: "graphql",
		})
		if err != nil {
			log.Fatalf("Failed to create the Stackdriver exporter: %v", err)
		}
		defer sd.Flush()
		view.RegisterExporter(sd)
		trace.RegisterExporter(sd)
		trace.ApplyConfig(trace.Config{DefaultSampler: trace.AlwaysSample()})
	}

	isDev := os.Getenv("NAT_ENV") != "production"

	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Use(manager.SessionManager.Middleware)

	r.Use(cors.New(cors.Options{
		AllowCredentials:   true,
		OptionsPassthrough: true,
	}).Handler)

	r.NotFound(NotFoundHandler)

	// Stuff that does not ssl redirect
	r.Group(func(r chi.Router) {
		r.Use(secure.New(secure.Options{
			HostsProxyHeaders:  []string{"X-Forwarded-Host"},
			FrameDeny:          true,
			ContentTypeNosniff: true,
			BrowserXssFilter:   true,
			IsDevelopment:      isDev,
		}).Handler)

		r.Get("/healthz", healthCheckHandler)
		r.Handle("/metrics", pe)
	})

	// Everything that does SSL only
	r.Group(func(r chi.Router) {
		r.Use(secure.New(secure.Options{
			HostsProxyHeaders:  []string{"X-Forwarded-Host"},
			FrameDeny:          true,
			ContentTypeNosniff: true,
			BrowserXssFilter:   true,
			STSSeconds:         86400,
			SSLRedirect:        true,
			IsDevelopment:      isDev,
		}).Handler)

		r.Mount("/auth", Auth.NewServeMux())
		r.Mount("/admin", adminRouter())

		r.Handle("/", handler.Playground("graphql", "/graphql"))
		r.Handle("/graphql", handler.GraphQL(
			graphql.NewExecutableSchema(graphql.New()),
			handler.RecoverFunc(func(ctx context.Context, err interface{}) error {
				log.Print(err)
				debug.PrintStack()
				return errors.New("Panic message seen when processing request")
			}),
		))
	})

	h := &ochttp.Handler{
		Handler: r,
	}
	if err := view.Register(ochttp.DefaultServerViews...); err != nil {
		log.Fatal("Failed to register ochttp.DefaultServerViews")
	}

	log.Fatal(http.ListenAndServe(":"+port, h))
}

func adminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(AdminOnly)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		Renderer.HTML(w, http.StatusOK, "admin", nil)
	})

	return r
}

func AdminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//ctx := r.Context()
		//perm, ok := ctx.Value("acl.permission").(YourPermissionType)
		//if !ok || !perm.IsAdmin() {
		//	http.Error(w, http.StatusText(403), 403)
		//	return
		//}
		next.ServeHTTP(w, r)
	})
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	Renderer.JSON(w, http.StatusOK, map[string]string{
		"healthy":  "true",
		"revision": os.Getenv("GIT_REVISION"),
		"tag":      os.Getenv("GIT_TAG"),
		"branch":   os.Getenv("GIT_BRANCH"),
	})
}

func NotFoundHandler(w http.ResponseWriter, r *http.Request) {
	Renderer.HTML(w, http.StatusNotFound, "404", struct{ Title string }{Title: "404: This page could not be found"})
}
