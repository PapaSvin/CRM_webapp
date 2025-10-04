// Postgres-only build (default)

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"

	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/xuri/excelize/v2"
)

var db *sql.DB
var pgSchema string
var cookieDomain string
var cookieSecure bool = false
var supplierHasArrays bool // suppliers.phone_ids/email_ids существуют
var adminPassword string
var checkoAPIKey string

func loadEnv() {
	if err := godotenv.Load(); err != nil {
		log.Println("warn: .env not found, using system environment")
	}
	// Load admin password from ENV
	adminPassword = strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))
	if adminPassword == "" {
		// No fatal here; allow setting later, but warn.
		log.Println("warn: ADMIN_PASSWORD is not set; /apanel will be inaccessible until configured")
	}
	// Load Checko API key
	checkoAPIKey = strings.TrimSpace(os.Getenv("CHECKO_API_KEY"))
	if checkoAPIKey == "" {
		log.Println("warn: CHECKO_API_KEY is not set; /addcompany will not work without it")
	}
}

func loadCookieConfig() {
	cookieDomain = strings.TrimSpace(os.Getenv("COOKIE_DOMAIN"))
	if cookieDomain == "" {
		cookieDomain = ""
	}
	cd := strings.ToLower(strings.TrimSpace(os.Getenv("COOKIE_SECURE")))
	if cd == "1" || cd == "true" || cd == "yes" {
		cookieSecure = true
	} else {
		cookieSecure = false
	}
}

// Определяем, пришёл ли запрос по HTTPS (учитываем прокси)
func isHTTPS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	// За прокси обычно ставят X-Forwarded-Proto/ X-Forwarded-Scheme
	if p := c.GetHeader("X-Forwarded-Proto"); p != "" {
		p = strings.TrimSpace(strings.SplitN(p, ",", 2)[0])
		if strings.EqualFold(p, "https") {
			return true
		}
	}
	if s := c.GetHeader("X-Forwarded-Scheme"); s != "" {
		s = strings.TrimSpace(strings.SplitN(s, ",", 2)[0])
		if strings.EqualFold(s, "https") {
			return true
		}
	}
	if c.GetHeader("X-Forwarded-Ssl") == "on" {
		return true
	}
	return false
}

// Вычисляет домен для cookie в зависимости от настроек и хоста запроса
func cookieDomainForRequest(c *gin.Context) string {
	// Если явно не задан, используем host-only cookie (без Domain атрибута)
	if cookieDomain == "" {
		return ""
	}
	// Специальный режим: COOKIE_DOMAIN=auto — подставляем хост из запроса
	if strings.EqualFold(cookieDomain, "auto") {
		host := c.Request.Host
		if host == "" {
			return ""
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSpace(host)
		// Для localhost и IP — лучше host-only cookie
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || net.ParseIP(host) != nil {
			return ""
		}
		return host
	}
	// Если явно указан localhost, но реальный хост не localhost — используем host-only
	if strings.EqualFold(cookieDomain, "localhost") {
		host := c.Request.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSpace(host)
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return "localhost"
		}
		return "" // не задаём Domain, иначе браузер отбросит cookie
	}
	// Если установлен localhost, а доступ не по localhost, браузер может не прислать cookie.
	// Оставляем как есть, но лучше на проде не ставить COOKIE_DOMAIN=localhost.
	return cookieDomain
}

func setUserCookie(c *gin.Context, username string) {
	// Делать Secure только если реальный протокол HTTPS; иначе браузер не сохранит cookie
	sec := cookieSecure && isHTTPS(c)
	dom := cookieDomainForRequest(c)
	// Явно укажем SameSite=Lax, чтобы cookie гарантированно отправлялась при редиректе после POST
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("user", username, 3600, "/", dom, sec, true)
}

func clearUserCookie(c *gin.Context) {
	sec := cookieSecure && isHTTPS(c)
	dom := cookieDomainForRequest(c)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("user", "", -1, "/", dom, sec, true)
}

// Admin cookie helpers (separate cookie from user)
func setAdminCookie(c *gin.Context, username string) {
	sec := cookieSecure && isHTTPS(c)
	dom := cookieDomainForRequest(c)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("admin", username, 3600, "/", dom, sec, true)
}

func clearAdminCookie(c *gin.Context) {
	sec := cookieSecure && isHTTPS(c)
	dom := cookieDomainForRequest(c)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("admin", "", -1, "/", dom, sec, true)
}

func buildPostgresDSN() string {
	user := os.Getenv("POSTGRES_USER")
	pass := os.Getenv("POSTGRES_PASSWORD")
	host := os.Getenv("POSTGRES_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("POSTGRES_PORT")
	if port == "" {
		port = "5432"
	}
	dbname := os.Getenv("POSTGRES_DB")
	if user == "" || pass == "" || dbname == "" {
		log.Fatal("POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_DB must be set")
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, pass, host, port, dbname)
}

func schemaPrefix() string {
	if pgSchema == "" {
		return ""
	}
	return fmt.Sprintf("\"%s\".", pgSchema)
}

func initDB() {
	var err error
	dsn := buildPostgresDSN()
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	if err = db.Ping(); err != nil {
		log.Fatal(err)
	}

	pgSchema = os.Getenv("POSTGRES_SCHEMA")
	if pgSchema == "" {
		pgSchema = "public"
	}
	if _, err := db.Exec(fmt.Sprintf("SET search_path TO \"%s\"", pgSchema)); err != nil {
		log.Printf("warn: failed to set search_path: %v", err)
	}
}

// Schedule a daily pg_dump at 23:59 local time, overwriting a fixed file
func scheduleDailyDump() {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			d := time.Until(next)
			timer := time.NewTimer(d)
			<-timer.C
			// run dump
			if err := runPgDump("dump-postgres-latest.sql"); err != nil {
				log.Printf("warn: pg_dump failed: %v", err)
			} else {
				log.Printf("info: pg_dump completed: dump-postgres-latest.sql")
			}
		}
	}()
}

// Executes pg_dump using POSTGRES_* env settings; writes to given file path
func runPgDump(outFile string) error {
	user := strings.TrimSpace(os.Getenv("POSTGRES_USER"))
	pass := os.Getenv("POSTGRES_PASSWORD")
	host := strings.TrimSpace(os.Getenv("POSTGRES_HOST"))
	if host == "" {
		host = "localhost"
	}
	port := strings.TrimSpace(os.Getenv("POSTGRES_PORT"))
	if port == "" {
		port = "5432"
	}
	dbname := strings.TrimSpace(os.Getenv("POSTGRES_DB"))
	if user == "" || pass == "" || dbname == "" {
		return fmt.Errorf("POSTGRES_USER/POSTGRES_PASSWORD/POSTGRES_DB must be set for pg_dump")
	}
	args := []string{"-h", host, "-p", port, "-U", user, "-d", dbname, "-F", "p", "--no-owner", "--no-privileges", "-f", outFile}
	if pgSchema != "" {
		args = append(args, "-n", pgSchema)
	}
	cmd := exec.Command("pg_dump", args...)
	// pass password to pg_dump
	cmd.Env = append(os.Environ(), "PGPASSWORD="+pass)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_dump error: %v; output: %s", err, string(out))
	}
	return nil
}

// Проверяем, существуют ли в таблице suppliers колонки phone_ids и email_ids
func detectSupplierArrays() {
	// Пытаемся обратиться к колонкам напрямую; LIMIT 0 не вернёт строк, но проверит схему
	q := fmt.Sprintf("SELECT phone_ids, email_ids FROM %ssuppliers LIMIT 0", schemaPrefix())
	rows, err := db.Query(q)
	if err != nil {
		// Ожидаемая ошибка при отсутствии колонок: 42703 (undefined_column)
		if !strings.Contains(err.Error(), "42703") {
			log.Printf("warn: detectSupplierArrays unexpected error: %v", err)
		}
		supplierHasArrays = false
		log.Printf("info: suppliers arrays present: %v", supplierHasArrays)
		return
	}
	// Если запрос выполнился, колонки существуют
	_ = rows.Close()
	supplierHasArrays = true
	log.Printf("info: suppliers arrays present: %v", supplierHasArrays)
}

// 	// Загружаем дамп, если указан
// 	dumpFile := strings.TrimSpace(os.Getenv("POSTGRES_DUMP"))
// 	if dumpFile == "" {
// 		dumpFile = "dump-postgres-202509152112.sql"
// 	}
// 	if _, err := os.Stat(dumpFile); err == nil {
// 		cmd := exec.Command("psql", buildPostgresDSN(), "-f", dumpFile)
// 		if out, err := cmd.CombinedOutput(); err != nil {
// 			log.Printf("warn: failed to import dump %s: %v\n%s", dumpFile, err, out)
// 		} else {
// 			log.Printf("info: imported dump %s", dumpFile)
// 		}
// 	} else {
// 		log.Printf("warn: dump file %s not found", dumpFile)
// 	}
// }

func authRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		username, err := c.Cookie("user")
		if err != nil || username == "" {
			log.Printf("authRequired: no user cookie: %v", err)
			c.Redirect(http.StatusFound, "/login")
			c.Abort()
			return
		}
		c.Set("user", username)
		c.Next()
	}
}

func main() {
	loadEnv()
	initDB()
	loadCookieConfig()
	detectSupplierArrays()
	// start daily DB dump scheduler
	scheduleDailyDump()

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.LoadHTMLGlob("templates/*.html")
	r.Static("/static", "./static")

	// helper: require admin cookie
	requireAdmin := func(c *gin.Context) bool {
		adminUser, _ := c.Cookie("admin")
		if adminUser == "" {
			c.Redirect(http.StatusFound, "/apanel")
			return false
		}
		return true
	}

	// Главная
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/login")
	})

	// Логин
	r.GET("/login", func(c *gin.Context) {
		c.HTML(http.StatusOK, "login.html", gin.H{"error": ""})
	})

	r.POST("/login", func(c *gin.Context) {
		username := strings.TrimSpace(c.PostForm("username"))
		uname := strings.ToLower(username)
		password := c.PostForm("password")

		q := fmt.Sprintf("SELECT password_hash FROM %susers WHERE lower(username)=lower($1)", schemaPrefix())
		var hash sql.NullString
		err := db.QueryRow(q, uname).Scan(&hash)
		if err != nil {
			c.HTML(http.StatusUnauthorized, "login.html", gin.H{"error": "Пользователь не найден"})
			return
		}
		if !hash.Valid || bcrypt.CompareHashAndPassword([]byte(hash.String), []byte(password)) != nil {
			c.HTML(http.StatusUnauthorized, "login.html", gin.H{"error": "Неверный пароль"})
			return
		}

		// Успешный вход
		setUserCookie(c, username)
		c.Redirect(http.StatusFound, "/companies")
	})

	// Регистрация
	r.GET("/register", func(c *gin.Context) {
		c.HTML(http.StatusOK, "register.html", gin.H{"error": ""})
	})

	r.POST("/register", func(c *gin.Context) {
		username := strings.TrimSpace(c.PostForm("username"))
		uname := strings.ToLower(username)
		password := c.PostForm("password")
		hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)

		ins := fmt.Sprintf("INSERT INTO %susers(username, password_hash) VALUES ($1,$2)", schemaPrefix())
		_, err := db.Exec(ins, uname, hash)
		if err != nil {
			msg := "Ошибка базы"
			es := err.Error()
			if strings.Contains(es, "23505") || strings.Contains(strings.ToLower(es), "duplicate key") {
				msg = "Пользователь уже существует"
			}
			c.HTML(http.StatusBadRequest, "register.html", gin.H{"error": msg})
			return
		}
		c.Redirect(http.StatusFound, "/login")
	})

	// Выход
	r.GET("/logout", func(c *gin.Context) {
		clearUserCookie(c)
		c.Redirect(http.StatusFound, "/login")
	})

	// Профиль
	r.GET("/profile", authRequired(), func(c *gin.Context) {
		username := c.MustGet("user").(string)
		c.HTML(http.StatusOK, "profile.html", gin.H{"username": username})
	})

	// Admin panel: password-gated access reachable from profile
	// GET: shows form when not admin-authenticated; shows panel when authenticated
	r.GET("/apanel", authRequired(), func(c *gin.Context) {
		// Must be logged in as a normal user first
		username := c.MustGet("user").(string)
		adminUser, _ := c.Cookie("admin")
		if adminUser == "" {
			// show password form
			c.HTML(http.StatusOK, "apanel.html", gin.H{
				"username": username,
				"isAdmin":  false,
				"error":    strings.TrimSpace(c.Query("error")),
			})
			return
		}
		// already admin
		c.HTML(http.StatusOK, "apanel.html", gin.H{
			"username": username,
			"isAdmin":  true,
		})
	})

	// POST: verify password, set admin cookie
	r.POST("/apanel", authRequired(), func(c *gin.Context) {
		if adminPassword == "" {
			c.HTML(http.StatusForbidden, "apanel.html", gin.H{
				"isAdmin": false,
				"error":   "Админ пароль не настроен",
			})
			return
		}
		pass := c.PostForm("password")
		// Simple exact match; could be enhanced to hash-based
		if pass != adminPassword {
			c.HTML(http.StatusUnauthorized, "apanel.html", gin.H{
				"isAdmin": false,
				"error":   "Неверный пароль",
			})
			return
		}
		username := c.MustGet("user").(string)
		setAdminCookie(c, username)
		c.Redirect(http.StatusFound, "/apanel")
	})

	// Admin sign-out (only clears the admin cookie; user session remains)
	r.GET("/apanel/logout", authRequired(), func(c *gin.Context) {
		clearAdminCookie(c)
		c.Redirect(http.StatusFound, "/apanel")
	})

	// DelCompany page: GET shows form, POST performs delete
	r.GET("/delcompany", authRequired(), func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		// Load company options
		optsSQL := fmt.Sprintf("SELECT DISTINCT company FROM %ssuppliers WHERE company IS NOT NULL AND company<>'' ORDER BY company LIMIT 200", schemaPrefix())
		optRows, err := db.Query(optsSQL)
		if err != nil {
			c.HTML(http.StatusOK, "delcompany.html", gin.H{"error": err.Error(), "companyOptions": []string{}})
			return
		}
		defer optRows.Close()
		var companyOptions []string
		for optRows.Next() {
			var v sql.NullString
			if err := optRows.Scan(&v); err == nil && v.Valid && strings.TrimSpace(v.String) != "" {
				companyOptions = append(companyOptions, v.String)
			}
		}
		c.HTML(http.StatusOK, "delcompany.html", gin.H{
			"companyOptions": companyOptions,
			"msg":            strings.TrimSpace(c.Query("msg")),
			"error":          strings.TrimSpace(c.Query("error")),
		})
	})

	r.POST("/delcompany", authRequired(), func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		company := strings.TrimSpace(c.PostForm("company"))
		inn := strings.TrimSpace(c.PostForm("inn"))
		// Helper to rerender with options and message
		renderWith := func(msg, errMsg string) {
			optsSQL := fmt.Sprintf("SELECT DISTINCT company FROM %ssuppliers WHERE company IS NOT NULL AND company<>'' ORDER BY company LIMIT 200", schemaPrefix())
			optRows, err := db.Query(optsSQL)
			if err != nil {
				c.HTML(http.StatusOK, "delcompany.html", gin.H{"error": err.Error(), "companyOptions": []string{}})
				return
			}
			defer optRows.Close()
			var companyOptions []string
			for optRows.Next() {
				var v sql.NullString
				if err := optRows.Scan(&v); err == nil && v.Valid && strings.TrimSpace(v.String) != "" {
					companyOptions = append(companyOptions, v.String)
				}
			}
			c.HTML(http.StatusOK, "delcompany.html", gin.H{"companyOptions": companyOptions, "msg": msg, "error": errMsg})
		}
		if company == "" || inn == "" {
			renderWith("", "Укажите представительство и ИНН")
			return
		}
		tx, err := db.Begin()
		if err != nil {
			renderWith("", err.Error())
			return
		}
		defer func() {
			if err != nil {
				_ = tx.Rollback()
			}
		}()
		rows, err := tx.Query(fmt.Sprintf("SELECT id FROM %ssuppliers WHERE company=$1 AND inn=$2", schemaPrefix()), company, inn)
		if err != nil {
			renderWith("", err.Error())
			return
		}
		var ids []int
		for rows.Next() {
			var id int
			_ = rows.Scan(&id)
			if id > 0 {
				ids = append(ids, id)
			}
		}
		_ = rows.Close()
		if len(ids) == 0 {
			_ = tx.Rollback()
			renderWith("", "Компания не найдена для удаления")
			return
		}
		for _, id := range ids {
			if _, err = tx.Exec(fmt.Sprintf("DELETE FROM %ssupplier_phones WHERE supplier_id=$1", schemaPrefix()), id); err != nil {
				renderWith("", err.Error())
				return
			}
			if _, err = tx.Exec(fmt.Sprintf("DELETE FROM %ssupplier_emails WHERE supplier_id=$1", schemaPrefix()), id); err != nil {
				renderWith("", err.Error())
				return
			}
			if _, err = tx.Exec(fmt.Sprintf("DELETE FROM %ssupplier_change_log WHERE supplier_id=$1", schemaPrefix()), id); err != nil {
				renderWith("", err.Error())
				return
			}
			if _, err = tx.Exec(fmt.Sprintf("DELETE FROM %ssuppliers WHERE id=$1", schemaPrefix()), id); err != nil {
				renderWith("", err.Error())
				return
			}
		}
		if err = tx.Commit(); err != nil {
			renderWith("", err.Error())
			return
		}
		renderWith(fmt.Sprintf("Удалено записей: %d (ИНН %s, представительство %s)", len(ids), inn, company), "")
	})

	// Removed inline delete route; moved to /delcompany page

	// ========== Add Company via Checko ==========
	// Small struct to hold parsed Checko data
	type checkoData struct {
		Name   string
		Phones []string
		Emails []string
		Site   string
	}

	fetchChecko := func(inn string) (checkoData, error) {
		var out checkoData
		if checkoAPIKey == "" {
			return out, fmt.Errorf("CHECKO_API_KEY not set")
		}
		url := fmt.Sprintf("https://api.checko.ru/v2/company?key=%s&inn=%s", checkoAPIKey, strings.TrimSpace(inn))
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			return out, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var j any
		if err := json.Unmarshal(body, &j); err != nil {
			// naive fallback to <pre> JSON
			b := string(body)
			lower := strings.ToLower(b)
			start := strings.Index(lower, "<pre")
			if start >= 0 {
				end := strings.Index(lower[start:], "</pre>")
				if end > 0 {
					frag := b[start : start+end]
					gt := strings.Index(frag, ">")
					if gt >= 0 {
						jsonText := frag[gt+1:]
						_ = json.Unmarshal([]byte(jsonText), &j)
					}
				}
			}
		}
		m, ok := j.(map[string]any)
		if !ok {
			return out, fmt.Errorf("unexpected response")
		}
		data, _ := m["data"].(map[string]any)
		if data == nil {
			return out, fmt.Errorf("no data in response")
		}
		if v, ok := data["НаимПолн"].(string); ok && strings.TrimSpace(v) != "" {
			out.Name = strings.TrimSpace(v)
		}
		contacts, _ := data["Контакты"].(map[string]any)
		if contacts != nil {
			if ph, ok := contacts["Тел"].([]any); ok {
				for _, p := range ph {
					if s, ok := p.(string); ok && strings.TrimSpace(s) != "" {
						out.Phones = append(out.Phones, strings.TrimSpace(s))
					}
				}
			}
			if em, ok := contacts["Емэйл"].([]any); ok {
				for _, e := range em {
					if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
						out.Emails = append(out.Emails, strings.TrimSpace(s))
					}
				}
			}
			if ws, ok := contacts["ВебСайт"].(string); ok && strings.TrimSpace(ws) != "" {
				out.Site = strings.TrimSpace(ws)
			}
		}
		return out, nil
	}

	// GET: show form (admin only)
	r.GET("/addcompany", authRequired(), func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		// Build company options like elsewhere
		optsSQL := fmt.Sprintf("SELECT DISTINCT company FROM %ssuppliers WHERE company IS NOT NULL AND company<>'' ORDER BY company LIMIT 200", schemaPrefix())
		optRows, err := db.Query(optsSQL)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		defer optRows.Close()
		var companyOptions []string
		for optRows.Next() {
			var v sql.NullString
			_ = optRows.Scan(&v)
			if v.Valid && strings.TrimSpace(v.String) != "" {
				companyOptions = append(companyOptions, v.String)
			}
		}
		c.HTML(http.StatusOK, "addcompany.html", gin.H{"error": "", "companyOptions": companyOptions})
	})

	// POST: fetch from Checko and insert supplier
	r.POST("/addcompany", authRequired(), func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		inn := strings.TrimSpace(c.PostForm("inn"))
		companyName := strings.TrimSpace(c.PostForm("company"))
		if inn == "" {
			c.HTML(http.StatusBadRequest, "addcompany.html", gin.H{"error": "Введите ИНН", "companyOptions": []string{}})
			return
		}
		info, err := fetchChecko(inn)
		if err != nil || info.Name == "" {
			c.HTML(http.StatusOK, "addcompany.html", gin.H{"error": fmt.Sprintf("Не удалось получить данные: %v", err), "companyOptions": []string{}})
			return
		}
		// Check duplicate by INN
		var existingID int
		err = db.QueryRow(fmt.Sprintf("SELECT id FROM %ssuppliers WHERE inn=$1 LIMIT 1", schemaPrefix()), inn).Scan(&existingID)
		if err == nil && existingID > 0 {
			c.Redirect(http.StatusFound, fmt.Sprintf("/company/%d", existingID))
			return
		}
		// Insert supplier
		phonesStr := strings.Join(info.Phones, ", ")
		emailsStr := strings.Join(info.Emails, ", ")
		insSQL := fmt.Sprintf("INSERT INTO %ssuppliers(name, inn, phones, email, site, comment, status, company, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NOW(),NOW()) RETURNING id", schemaPrefix())
		var newID int
		err = db.QueryRow(insSQL, info.Name, inn, phonesStr, emailsStr, info.Site, "", "new", companyName).Scan(&newID)
		if err != nil {
			c.HTML(http.StatusOK, "addcompany.html", gin.H{"error": fmt.Sprintf("Ошибка сохранения: %v", err), "companyOptions": []string{}})
			return
		}
		// Insert normalized phones/emails
		for _, p := range info.Phones {
			_, _ = db.Exec(fmt.Sprintf("INSERT INTO %ssupplier_phones(supplier_id, phone, called, comment) VALUES ($1,$2,false,'')", schemaPrefix()), newID, p)
		}
		for _, e := range info.Emails {
			_, _ = db.Exec(fmt.Sprintf("INSERT INTO %ssupplier_emails(supplier_id, email, written, comment) VALUES ($1,$2,false,'')", schemaPrefix()), newID, e)
		}
		c.Redirect(http.StatusFound, fmt.Sprintf("/company/%d", newID))
	})

	// Компании (список + поиск) — из таблицы suppliers в Postgres
	r.GET("/companies", authRequired(), func(c *gin.Context) {
		filterStatus := c.Query("status")
		q := strings.TrimSpace(c.Query("q"))

		base := fmt.Sprintf("SELECT id, COALESCE(name,''), COALESCE(inn,''), COALESCE(phones,''), COALESCE(email,''), COALESCE(site,''), COALESCE(comment,''), COALESCE(status,'new'), COALESCE(company,'') FROM %ssuppliers", schemaPrefix())
		conds := []string{}
		args := []any{}

		if filterStatus != "" && filterStatus != "all" {
			conds = append(conds, "status=$1")
			args = append(args, filterStatus)
		}
		// Фильтр по полю company (частичное совпадение)
		companyFilter := strings.TrimSpace(c.Query("company"))
		if companyFilter == "" {
			companyFilter = strings.TrimSpace(c.Query("company_select"))
		}
		if companyFilter != "" {
			idx := len(args) + 1
			conds = append(conds, fmt.Sprintf("company ILIKE $%d", idx))
			args = append(args, "%"+companyFilter+"%")
		}
		if q != "" {
			// сдвигаем нумерацию плейсхолдеров в зависимости от уже добавленных аргументов
			baseIdx := len(args) + 1
			conds = append(conds, fmt.Sprintf("(name ILIKE $%d OR inn ILIKE $%d OR email ILIKE $%d OR site ILIKE $%d OR phones ILIKE $%d)", baseIdx, baseIdx+1, baseIdx+2, baseIdx+3, baseIdx+4))
			like := "%" + q + "%"
			args = append(args, like, like, like, like, like)
		}
		if len(conds) > 0 {
			base += " WHERE " + strings.Join(conds, " AND ")
		}
		base += " ORDER BY name"

		rows, err := db.Query(base, args...)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()

		var companies []map[string]any
		for rows.Next() {
			var id int
			var name, inn, phones, email, site, comment, st, company string
			if err := rows.Scan(&id, &name, &inn, &phones, &email, &site, &comment, &st, &company); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
			companies = append(companies, gin.H{
				"id":      id,
				"name":    name,
				"inn":     inn,
				"phones":  phones,
				"email":   email,
				"site":    site,
				"comment": comment,
				"status":  st,
				"company": company,
			})
		}
		// Для выпадающего списка company
		optsSQL := fmt.Sprintf("SELECT DISTINCT company FROM %ssuppliers WHERE company IS NOT NULL AND company<>''", schemaPrefix())
		optConds := []string{}
		optArgs := []any{}
		idx := 1
		if filterStatus != "" && filterStatus != "all" {
			optConds = append(optConds, fmt.Sprintf("status=$%d", idx))
			optArgs = append(optArgs, filterStatus)
			idx++
		}
		if len(optConds) > 0 {
			optsSQL += " AND " + strings.Join(optConds, " AND ")
		}
		optsSQL += " ORDER BY company LIMIT 200"
		optRows, err := db.Query(optsSQL, optArgs...)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		defer optRows.Close()
		var companyOptions []string
		for optRows.Next() {
			var v sql.NullString
			if err := optRows.Scan(&v); err != nil {
				break
			}
			if v.Valid {
				companyOptions = append(companyOptions, v.String)
			}
		}

		c.HTML(http.StatusOK, "companies.html", gin.H{
			"companies":      companies,
			"status":         filterStatus,
			"q":              q,
			"company":        companyFilter,
			"companyOptions": companyOptions,
		})
	})

	// Подсказки для поиска (autocomplete)
	r.GET("/api/suggest", authRequired(), func(c *gin.Context) {
		q := strings.TrimSpace(c.Query("q"))
		if q == "" {
			c.JSON(http.StatusOK, []any{})
			return
		}
		like := "%" + q + "%"
		qsql := fmt.Sprintf(`
			SELECT id, COALESCE(name,''), COALESCE(inn,'')
			FROM %ssuppliers
			WHERE name ILIKE $1 OR inn ILIKE $2 OR email ILIKE $3 OR site ILIKE $4 OR phones ILIKE $5
			ORDER BY CASE WHEN name ILIKE $6 THEN 0 ELSE 1 END, name
			LIMIT 10
		`, schemaPrefix())
		rows, err := db.Query(qsql, like, like, like, like, like, like)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		type item struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
			INN  string `json:"inn"`
		}
		var out []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.ID, &it.Name, &it.INN); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			out = append(out, it)
		}
		c.JSON(http.StatusOK, out)
	})

	// Страница компании
	r.GET("/company/:id", authRequired(), func(c *gin.Context) {
		idStr := c.Param("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			c.String(http.StatusBadRequest, "Некорректный ID")
			return
		}

		var company struct {
			ID      int
			Name    string
			INN     string
			Site    string
			Comment string
			Status  string
			Company string
		}

		phones := []map[string]any{}
		emails := []map[string]any{}

		// Helper to mark favorites based on a simple convention in comment
		isFav := func(comment string) bool {
			c := strings.TrimSpace(strings.ToLower(comment))
			return strings.HasPrefix(c, "[*]") || strings.HasPrefix(c, "[fav]") || strings.HasPrefix(c, "⭐")
		}

		// Helper to strip favorite markers for display
		stripFav := func(raw string) string {
			trimmed := strings.TrimSpace(raw)
			for {
				lower := strings.ToLower(trimmed)
				if strings.HasPrefix(lower, "[*]") {
					trimmed = strings.TrimSpace(trimmed[3:])
					continue
				}
				if strings.HasPrefix(lower, "[fav]") {
					trimmed = strings.TrimSpace(trimmed[5:])
					continue
				}
				if strings.HasPrefix(trimmed, "⭐") {
					if len(trimmed) >= 2 {
						trimmed = strings.TrimSpace(trimmed[2:])
					} else {
						trimmed = ""
					}
					continue
				}
				break
			}
			return trimmed
		}

		if supplierHasArrays {
			// Вариант со столбцами phone_ids/email_ids в suppliers
			qsql := fmt.Sprintf(`
				SELECT id, COALESCE(name,''), COALESCE(inn,''), COALESCE(site,''), COALESCE(comment,''), COALESCE(status,'new'),
					COALESCE(company,''), COALESCE(phone_ids,'{}'), COALESCE(email_ids,'{}')
				FROM %ssuppliers WHERE id = $1
			`, schemaPrefix())
			var phoneIDs, emailIDs []int32
			if err := db.QueryRow(qsql, id).Scan(
				&company.ID, &company.Name, &company.INN, &company.Site, &company.Comment, &company.Status,
				&company.Company, pq.Array(&phoneIDs), pq.Array(&emailIDs),
			); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
			// Подтянем фактические записи по спискам id (если таблицы есть)
			seenP := map[int]struct{}{}
			for _, pid := range phoneIDs {
				var phone string
				var called bool
				var comment sql.NullString
				if err := db.QueryRow(fmt.Sprintf("SELECT phone, called, comment FROM %ssupplier_phones WHERE id=$1", schemaPrefix()), pid).Scan(&phone, &called, &comment); err == nil {
					phones = append(phones, gin.H{"id": int(pid), "phone": phone, "called": called, "comment": stripFav(comment.String), "fav": isFav(comment.String)})
					seenP[int(pid)] = struct{}{}
				}
			}
			seenE := map[int]struct{}{}
			for _, eid := range emailIDs {
				var email string
				var written bool
				var comment sql.NullString
				if err := db.QueryRow(fmt.Sprintf("SELECT email, written, comment FROM %ssupplier_emails WHERE id=$1", schemaPrefix()), eid).Scan(&email, &written, &comment); err == nil {
					emails = append(emails, gin.H{"id": int(eid), "email": email, "written": written, "comment": stripFav(comment.String), "fav": isFav(comment.String)})
					seenE[int(eid)] = struct{}{}
				}
			}
			// Дополнительно подгрузим все записи по supplier_id и объединим (на случай пустых массивов или рассинхрона)
			pRows2, perr2 := db.Query(fmt.Sprintf("SELECT id, phone, called, comment FROM %ssupplier_phones WHERE supplier_id=$1 ORDER BY id", schemaPrefix()), company.ID)
			if perr2 == nil {
				defer pRows2.Close()
				for pRows2.Next() {
					var pid int
					var phone string
					var called bool
					var pcomment sql.NullString
					if err := pRows2.Scan(&pid, &phone, &called, &pcomment); err == nil {
						if _, ok := seenP[pid]; !ok {
							phones = append(phones, gin.H{"id": pid, "phone": phone, "called": called, "comment": stripFav(pcomment.String), "fav": isFav(pcomment.String)})
						}
					}
				}
			}
			eRows2, eerr2 := db.Query(fmt.Sprintf("SELECT id, email, written, comment FROM %ssupplier_emails WHERE supplier_id=$1 ORDER BY id", schemaPrefix()), company.ID)
			if eerr2 == nil {
				defer eRows2.Close()
				for eRows2.Next() {
					var eid int
					var email string
					var written bool
					var ecomment sql.NullString
					if err := eRows2.Scan(&eid, &email, &written, &ecomment); err == nil {
						if _, ok := seenE[eid]; !ok {
							emails = append(emails, gin.H{"id": eid, "email": email, "written": written, "comment": stripFav(ecomment.String), "fav": isFav(ecomment.String)})
						}
					}
				}
			}
		} else {
			// Нормализованный вариант: тянем по supplier_id
			qsql := fmt.Sprintf(`
				SELECT id, COALESCE(name,''), COALESCE(inn,''), COALESCE(site,''), COALESCE(comment,''), COALESCE(status,'new'),
					COALESCE(company,'')
				FROM %ssuppliers WHERE id = $1
			`, schemaPrefix())
			if err := db.QueryRow(qsql, id).Scan(
				&company.ID, &company.Name, &company.INN, &company.Site, &company.Comment, &company.Status,
				&company.Company,
			); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
			pRows, perr := db.Query(fmt.Sprintf("SELECT id, phone, called, comment FROM %ssupplier_phones WHERE supplier_id=$1 ORDER BY id", schemaPrefix()), company.ID)
			if perr == nil {
				defer pRows.Close()
				for pRows.Next() {
					var pid int
					var phone string
					var called bool
					var pcomment sql.NullString
					if err := pRows.Scan(&pid, &phone, &called, &pcomment); err == nil {
						phones = append(phones, gin.H{"id": pid, "phone": phone, "called": called, "comment": stripFav(pcomment.String), "fav": isFav(pcomment.String)})
					}
				}
			}
			eRows, eerr := db.Query(fmt.Sprintf("SELECT id, email, written, comment FROM %ssupplier_emails WHERE supplier_id=$1 ORDER BY id", schemaPrefix()), company.ID)
			if eerr == nil {
				defer eRows.Close()
				for eRows.Next() {
					var eid int
					var email string
					var written bool
					var ecomment sql.NullString
					if err := eRows.Scan(&eid, &email, &written, &ecomment); err == nil {
						emails = append(emails, gin.H{"id": eid, "email": email, "written": written, "comment": stripFav(ecomment.String), "fav": isFav(ecomment.String)})
					}
				}
			}
		}

		// Сформируем список избранных контактов для верхней таблицы
		favorites := []map[string]any{}
		for _, p := range phones {
			if fav, ok := p["fav"].(bool); ok && fav {
				favorites = append(favorites, gin.H{"type": "Телефон", "value": p["phone"], "status": p["called"], "comment": p["comment"]})
			}
		}
		for _, e := range emails {
			if fav, ok := e["fav"].(bool); ok && fav {
				favorites = append(favorites, gin.H{"type": "Email", "value": e["email"], "status": e["written"], "comment": e["comment"]})
			}
		}

		// История изменений
		type change struct {
			ChangedAt  string
			ChangedBy  string
			OldStatus  string
			NewStatus  string
			OldComment string
			NewComment string
		}
		histSQL := fmt.Sprintf(`
			SELECT to_char(changed_at, 'YYYY-MM-DD HH24:MI:SS'), COALESCE(changed_by,''), 
				COALESCE(old_status,''), COALESCE(new_status,''), COALESCE(old_comment,''), COALESCE(new_comment,'')
			FROM %ssupplier_change_log 
			WHERE supplier_id=$1 
			ORDER BY changed_at DESC 
			LIMIT 200
		`, schemaPrefix())

		rows, err := db.Query(histSQL, id)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()

		history := []change{}
		for rows.Next() {
			var ch change
			if err := rows.Scan(&ch.ChangedAt, &ch.ChangedBy, &ch.OldStatus, &ch.NewStatus, &ch.OldComment, &ch.NewComment); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
			history = append(history, ch)
		}

		c.HTML(http.StatusOK, "company.html", gin.H{
			"company":   company,
			"history":   history,
			"phones":    phones,
			"emails":    emails,
			"favorites": favorites,
		})
		log.Printf("company %d: phones=%d, emails=%d (arrays=%v)", company.ID, len(phones), len(emails), supplierHasArrays)
	})

	r.POST("/company/:id/update", authRequired(), func(c *gin.Context) {
		id := c.Param("id")
		comment := c.PostForm("comment")
		status := c.PostForm("status")
		changedBy, _ := c.Cookie("user")

		tx, err := db.Begin()
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		defer func() {
			if err != nil {
				_ = tx.Rollback()
			}
		}()

		var oldStatus, oldComment sql.NullString
		selSQL := fmt.Sprintf("SELECT status, comment FROM %ssuppliers WHERE id=$1 FOR UPDATE", schemaPrefix())
		if err = tx.QueryRow(selSQL, id).Scan(&oldStatus, &oldComment); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}

		updSQL := fmt.Sprintf("UPDATE %ssuppliers SET comment=$1, status=$2, updated_at=NOW() WHERE id=$3", schemaPrefix())
		if _, err = tx.Exec(updSQL, comment, status, id); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}

		// Функция нормализации комментария с префиксом избранного
		normalizeFav := func(raw string, isFav bool) string {
			trimmed := strings.TrimSpace(raw)
			// Уберём существующие метки в начале
			for {
				lower := strings.ToLower(trimmed)
				if strings.HasPrefix(lower, "[*]") {
					trimmed = strings.TrimSpace(trimmed[3:])
					continue
				}
				if strings.HasPrefix(lower, "[fav]") {
					trimmed = strings.TrimSpace(trimmed[5:])
					continue
				}
				if strings.HasPrefix(trimmed, "⭐") {
					trimmed = strings.TrimSpace(trimmed[2:])
					continue
				}
				break
			}
			if isFav {
				return "[*] " + trimmed
			}
			return trimmed
		}

		// Обновляем телефоны
		processedP := map[string]bool{}
		for key, vals := range c.Request.PostForm {
			if strings.HasPrefix(key, "phone_called_") {
				pid := strings.TrimPrefix(key, "phone_called_")
				// take last value (hidden 0 + checkbox 1)
				v := vals[len(vals)-1]
				called := v == "1"
				phoneComment := c.PostForm("phone_comment_" + pid)
				// get last value for fav as well
				fv := c.Request.PostForm["phone_fav_"+pid]
				fav := len(fv) > 0 && fv[len(fv)-1] == "1"
				phoneComment = normalizeFav(phoneComment, fav)
				_, _ = tx.Exec("UPDATE supplier_phones SET called=$1, comment=$2 WHERE id=$3", called, phoneComment, pid)
				processedP[pid] = true
			}
		}
		// На случай, если called не менялся — но комментарий/избранное изменили
		for key := range c.Request.PostForm {
			if strings.HasPrefix(key, "phone_comment_") {
				pid := strings.TrimPrefix(key, "phone_comment_")
				if processedP[pid] {
					continue
				}
				phoneComment := c.PostForm("phone_comment_" + pid)
				fv := c.Request.PostForm["phone_fav_"+pid]
				fav := len(fv) > 0 && fv[len(fv)-1] == "1"
				phoneComment = normalizeFav(phoneComment, fav)
				// считаем called без изменений
				_, _ = tx.Exec("UPDATE supplier_phones SET comment=$1 WHERE id=$2", phoneComment, pid)
			}
		}

		// Обновляем почты
		processedE := map[string]bool{}
		for key, vals := range c.Request.PostForm {
			if strings.HasPrefix(key, "email_written_") {
				eid := strings.TrimPrefix(key, "email_written_")
				v := vals[len(vals)-1]
				written := v == "1"
				emailComment := c.PostForm("email_comment_" + eid)
				fv := c.Request.PostForm["email_fav_"+eid]
				fav := len(fv) > 0 && fv[len(fv)-1] == "1"
				emailComment = normalizeFav(emailComment, fav)
				_, _ = tx.Exec("UPDATE supplier_emails SET written=$1, comment=$2 WHERE id=$3", written, emailComment, eid)
				processedE[eid] = true
			}
		}
		for key := range c.Request.PostForm {
			if strings.HasPrefix(key, "email_comment_") {
				eid := strings.TrimPrefix(key, "email_comment_")
				if processedE[eid] {
					continue
				}
				emailComment := c.PostForm("email_comment_" + eid)
				fv := c.Request.PostForm["email_fav_"+eid]
				fav := len(fv) > 0 && fv[len(fv)-1] == "1"
				emailComment = normalizeFav(emailComment, fav)
				_, _ = tx.Exec("UPDATE supplier_emails SET comment=$1 WHERE id=$2", emailComment, eid)
			}
		}

		insLog := fmt.Sprintf(`
			INSERT INTO %ssupplier_change_log 
			(supplier_id, changed_at, changed_by, old_status, new_status, old_comment, new_comment, note)
			VALUES ($1, NOW(), $2, $3, $4, $5, $6, NULL)
		`, schemaPrefix())

		if _, err = tx.Exec(insLog, id, changedBy, oldStatus.String, status, oldComment.String, comment); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}

		if err = tx.Commit(); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Redirect(http.StatusFound, "/company/"+id)
	})

	// Страница экспорта и скачивание XLSX
	r.GET("/export", authRequired(), func(c *gin.Context) {
		// Если есть параметр download=1 — отдаём файл, иначе показываем страницу с формой
		if c.Query("download") == "1" {
			// Фильтры
			user := strings.TrimSpace(c.Query("user"))
			from := strings.TrimSpace(c.Query("from")) // YYYY-MM-DD
			to := strings.TrimSpace(c.Query("to"))     // YYYY-MM-DD
			status := strings.TrimSpace(c.Query("status"))
			companyName := strings.TrimSpace(c.Query("company"))
			if companyName == "" {
				companyName = strings.TrimSpace(c.Query("company_select"))
			}

			// Берём только последнее изменение по каждому supplier_id
			q := fmt.Sprintf(`
				WITH last_change AS (
					SELECT DISTINCT ON (supplier_id)
						   supplier_id, changed_at, changed_by, old_status, new_status, old_comment, new_comment
					FROM %ssupplier_change_log
					ORDER BY supplier_id, changed_at DESC
				)
				SELECT s.id as supplier_id,
				       COALESCE(s.name,'') AS name,
				       COALESCE(to_char(lc.changed_at, 'YYYY-MM-DD HH24:MI:SS'), '') AS changed_at,
				       COALESCE(lc.changed_by,'') AS changed_by,
				       COALESCE(lc.old_status,'') AS old_status,
				       COALESCE(lc.new_status,'') AS new_status,
				       COALESCE(lc.old_comment,'') AS old_comment,
				       COALESCE(lc.new_comment,'') AS new_comment
				FROM %ssuppliers s
				LEFT JOIN last_change lc ON lc.supplier_id = s.id
				WHERE 1=1
			`, schemaPrefix(), schemaPrefix())

			// Аргументы для плейсхолдеров
			args := []any{}

			// Применяем фильтры к текущему состоянию: user/from/to/status/company
			if user != "" {
				q += " AND lower(lc.changed_by)=lower($1)"
				args = append(args, user)
			}
			baseIdx := len(args) + 1
			if from != "" {
				q += fmt.Sprintf(" AND lc.changed_at >= $%d", baseIdx)
				args = append(args, from+" 00:00:00")
				baseIdx++
			}
			if to != "" {
				q += fmt.Sprintf(" AND lc.changed_at <= $%d", baseIdx)
				args = append(args, to+" 23:59:59")
				baseIdx++
			}
			if status != "" && status != "all" {
				q += fmt.Sprintf(" AND COALESCE(lc.new_status, s.status) = $%d", baseIdx)
				args = append(args, status)
				baseIdx++
			}
			if companyName != "" {
				q += fmt.Sprintf(" AND s.company ILIKE $%d", baseIdx)
				args = append(args, "%"+companyName+"%")
				baseIdx++
			}
			q += " ORDER BY s.name"

			rows, err := db.Query(q, args...)
			if err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
			defer rows.Close()

			type rec struct {
				SupplierID int
				Name       string
				ChangedAt  string
				ChangedBy  string
				OldStatus  string
				NewStatus  string
				OldComment string
				NewComment string
			}
			var data []rec
			for rows.Next() {
				var r rec
				if err := rows.Scan(&r.SupplierID, &r.Name, &r.ChangedAt, &r.ChangedBy, &r.OldStatus, &r.NewStatus, &r.OldComment, &r.NewComment); err != nil {
					c.String(http.StatusInternalServerError, err.Error())
					return
				}
				data = append(data, r)
			}

			// Генерация XLSX
			f := excelize.NewFile()
			// Лист Changes
			f.SetSheetName("Sheet1", "Changes")
			headers := []string{"Supplier ID", "Company", "Changed At", "Changed By", "Old Status", "New Status", "Old Comment", "New Comment"}
			for i, h := range headers {
				col, _ := excelize.CoordinatesToCellName(i+1, 1)
				_ = f.SetCellValue("Changes", col, h)
			}
			for i, rrow := range data {
				_ = f.SetCellValue("Changes", fmt.Sprintf("A%d", i+2), rrow.SupplierID)
				_ = f.SetCellValue("Changes", fmt.Sprintf("B%d", i+2), rrow.Name)
				_ = f.SetCellValue("Changes", fmt.Sprintf("C%d", i+2), rrow.ChangedAt)
				_ = f.SetCellValue("Changes", fmt.Sprintf("D%d", i+2), rrow.ChangedBy)
				_ = f.SetCellValue("Changes", fmt.Sprintf("E%d", i+2), rrow.OldStatus)
				_ = f.SetCellValue("Changes", fmt.Sprintf("F%d", i+2), rrow.NewStatus)
				_ = f.SetCellValue("Changes", fmt.Sprintf("G%d", i+2), rrow.OldComment)
				_ = f.SetCellValue("Changes", fmt.Sprintf("H%d", i+2), rrow.NewComment)
			}

			// Простая сводка по количеству обзвонов (new_status='called')
			_, _ = f.NewSheet("Summary")
			_ = f.SetCellValue("Summary", "A1", "Metric")
			_ = f.SetCellValue("Summary", "B1", "Value")

			// Подсчёт обзвонов в SQL по тем же фильтрам
			conds2 := []string{"new_status = 'called'"}
			args2 := []any{}
			idx2 := 1
			if user != "" {
				conds2 = append(conds2, fmt.Sprintf("lower(changed_by)=lower($%d)", idx2))
				args2 = append(args2, user)
				idx2++
			}
			if from != "" {
				conds2 = append(conds2, fmt.Sprintf("changed_at >= $%d", idx2))
				args2 = append(args2, from+" 00:00:00")
				idx2++
			}
			if to != "" {
				conds2 = append(conds2, fmt.Sprintf("changed_at <= $%d", idx2))
				args2 = append(args2, to+" 23:59:59")
				idx2++
			}
			cq := fmt.Sprintf("SELECT COUNT(*) FROM %ssupplier_change_log WHERE %s", schemaPrefix(), strings.Join(conds2, " AND "))
			var calledCount int
			if err := db.QueryRow(cq, args2...).Scan(&calledCount); err == nil {
				_ = f.SetCellValue("Summary", "A2", "Calls (status=called)")
				_ = f.SetCellValue("Summary", "B2", calledCount)
			}

			// Возврат файла
			c.Header("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
			c.Header("Content-Disposition", "attachment; filename=export.xlsx")
			c.Header("Content-Transfer-Encoding", "binary")
			// Записываем в ответ
			_ = f.Write(c.Writer)
			return
		}

		// Страница формы экспорта: подготовим список компаний
		optsSQL := fmt.Sprintf("SELECT DISTINCT company FROM %ssuppliers WHERE company IS NOT NULL AND company<>'' ORDER BY company LIMIT 200", schemaPrefix())
		optRows, err := db.Query(optsSQL)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		defer optRows.Close()
		var companyOptions []string
		for optRows.Next() {
			var v sql.NullString
			if err := optRows.Scan(&v); err == nil && v.Valid {
				companyOptions = append(companyOptions, v.String)
			}
		}
		c.HTML(http.StatusOK, "export.html", gin.H{"companyOptions": companyOptions})
	})

	r.Run(":8080")
}
