package main

import (
	"database/sql"
	"database/sql/driver"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
	"github.com/labstack/gommon/color"
	_ "github.com/mattn/go-sqlite3"
)

// Human-readable date-time format
const dateTime = "2006-01-02 15:04"

var authDisabled bool
var dbFile = "scan.db"

// NullTime "borrowed" from github.com/lib/pq

// NullTime represents a time.Time that may be null. NullTime implements the
// sql.Scanner interface so it can be used as a scan destination, similar to
// sql.NullString.
type NullTime struct {
	Time  time.Time
	Valid bool // Valid is true if Time is not NULL
}

// Scan implements the Scanner interface.
func (nt *NullTime) Scan(value interface{}) error {
	nt.Time, nt.Valid = value.(time.Time)
	return nil
}

// Value implements the Valuer interface.
func (nt NullTime) Value() (driver.Value, error) {
	if !nt.Valid {
		return nil, nil
	}
	return nt.Time, nil
}

// sqlFilter is for constructing data filters ("WHERE" clauses) in a SQL statement
type sqlFilter struct {
	Where  []string
	Values []interface{}
}

// String constructs a SQL WHERE clause.
func (f sqlFilter) String() string {
	if len(f.Where) > 0 {
		return "WHERE " + strings.Join(f.Where, " AND ")
	}
	return ""
}

type port struct {
	Port    int    `json:"port"`
	Proto   string `json:"proto"`
	Status  string `json:"status"`
	Service struct {
		Name   string `json:"name"`
		Banner string `json:"banner"`
	} `json:"service"`
}

// Results posted from masscan
type result struct {
	IP    string `json:"ip"`
	Ports []port `json:"ports"`
}

// Data retrieved from the database for display
type ipInfo struct {
	IP            string
	Port          int
	Proto         string
	FirstSeen     string
	LastSeen      string
	New           bool
	Gone          bool
	HasTraceroute bool
}

// Load all data for displaying in the browser
func loadData(filter sqlFilter) ([]ipInfo, error) {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return []ipInfo{}, err
	}
	defer db.Close()

	qry := fmt.Sprintf(`SELECT ip, port, proto, firstseen, lastseen FROM scan %s ORDER BY port, proto, ip, lastseen`, filter)
	rows, err := db.Query(qry, filter.Values...)
	if err != nil {
		return []ipInfo{}, err
	}

	defer rows.Close()

	var data []ipInfo
	var ip, proto string
	var firstseen, lastseen time.Time
	var port int
	var latest time.Time

	traceroutes, err := loadTraceroutes()
	if err != nil {
		return []ipInfo{}, err
	}

	for rows.Next() {
		err := rows.Scan(&ip, &port, &proto, &firstseen, &lastseen)
		if err != nil {
			return []ipInfo{}, err
		}
		if lastseen.After(latest) {
			latest = lastseen
		}
		var hasTraceroute bool
		if _, ok := traceroutes[ip]; ok {
			hasTraceroute = true
		}
		data = append(data, ipInfo{
			ip,
			port,
			proto,
			firstseen.Format(dateTime),
			lastseen.Format(dateTime),
			false,
			false,
			hasTraceroute})
	}

	for i := range data {
		f, _ := time.Parse(dateTime, data[i].FirstSeen)
		l, _ := time.Parse(dateTime, data[i].LastSeen)
		if f.Equal(l) && l == latest {
			data[i].New = true
		}
		if l.Before(latest) {
			data[i].Gone = true
		}
	}

	return data, nil
}

// Save the results posted
func saveData(results []result) (int64, error) {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	txn, err := db.Begin()
	if err != nil {
		return 0, err
	}

	insert, err := txn.Prepare(`INSERT INTO scan (ip, port, proto, firstseen, lastseen) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		txn.Rollback()
		return 0, err
	}
	qry, err := db.Prepare(`SELECT 1 FROM scan WHERE ip=? AND port=? AND proto=?`)
	if err != nil {
		txn.Rollback()
		return 0, err
	}
	update, err := txn.Prepare(`UPDATE scan SET lastseen=? WHERE ip=? AND port=? AND proto=?`)
	if err != nil {
		txn.Rollback()
		return 0, err
	}

	now := time.Now().UTC().Truncate(time.Minute)
	var count int64

	for _, r := range results {
		// Although it's an array, only one port is in each
		port := r.Ports[0]

		// Skip results which are (usually) banner-only
		// While it would be nice to store banners, we need to restructure a
		// bit to accommodate this and it just inserts duplicate data for now
		if port.Status == "" || port.Service.Name != "" {
			continue
		}

		// Search for the IP/port/proto combo
		// If it exists, update `lastseen`, else insert a new record

		// Because we have to scan into something
		var x int
		err := qry.QueryRow(r.IP, port.Port, port.Proto).Scan(&x)
		switch {
		case err == sql.ErrNoRows:
			_, err = insert.Exec(r.IP, port.Port, port.Proto, now, now)
			if err != nil {
				txn.Rollback()
				return 0, err
			}
			continue
		case err != nil:
			txn.Rollback()
			return 0, err
		}

		_, err = update.Exec(now, r.IP, port.Port, port.Proto)
		if err != nil {
			txn.Rollback()
			return 0, err
		}

		count++
	}

	txn.Commit()
	return count, nil
}

func loadTraceroutes() (map[string]struct{}, error) {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ips := make(map[string]struct{})

	rows, err := db.Query(`SELECT dest FROM traceroute`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ip string
	for rows.Next() {
		err := rows.Scan(&ip)
		if err != nil {
			return nil, err
		}
		if _, ok := ips[ip]; !ok {
			ips[ip] = struct{}{}
		}
	}

	return ips, nil
}

// Template is a template
type Template struct {
	templates *template.Template
}

// Render renders template
func (t *Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}

type indexData struct {
	Authenticated bool
	User          string
	scanData
}

type scanData struct {
	Total    int
	Latest   int
	New      int
	LastSeen string
	Results  []ipInfo
}

func resultData(ip, fs, ls string) (scanData, error) {
	var filter sqlFilter
	if ip != "" {
		filter.Where = append(filter.Where, `ip LIKE ?`)
		filter.Values = append(filter.Values, fmt.Sprintf("%%%s%%", ip))
	}
	if fs != "" {
		t, _ := time.Parse(dateTime, fs)
		filter.Where = append(filter.Where, `firstseen=?`)
		filter.Values = append(filter.Values, t)
	}
	if ls != "" {
		t, _ := time.Parse(dateTime, ls)
		filter.Where = append(filter.Where, `lastseen=?`)
		filter.Values = append(filter.Values, t)
	}

	results, err := loadData(filter)
	if err != nil {
		return scanData{}, err
	}

	data := scanData{
		Results: results,
		Total:   len(results),
	}

	// Find all the latest results and store the number in the struct
	var latest time.Time
	for _, r := range results {
		last, _ := time.Parse(dateTime, r.LastSeen)
		if last.After(latest) {
			latest = last
		}
	}
	for _, r := range results {
		last, _ := time.Parse(dateTime, r.LastSeen)
		if last.Equal(latest) {
			data.Latest++
		}
		if r.New {
			data.New++
		}
	}
	data.LastSeen = latest.Format(dateTime)

	return data, nil
}

// Handler for GET /
func index(c echo.Context) error {
	var user string
	if !authDisabled {
		session, err := store.Get(c.Request(), "user")
		if err != nil {
			return c.String(http.StatusInternalServerError, err.Error())
		}
		if _, ok := session.Values["user"]; !ok {
			data := indexData{}
			return c.Render(http.StatusOK, "index", data)
		}
		user = session.Values["user"].(string)
	}

	ip := c.QueryParam("ip")
	firstSeen := c.QueryParam("firstseen")
	lastSeen := c.QueryParam("lastseen")

	results, err := resultData(ip, firstSeen, lastSeen)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	data := indexData{Authenticated: true, User: user, scanData: results}

	return c.Render(http.StatusOK, "index", data)
}

// Handler for GET /ips.json
// This is used as the prefetch for Typeahead.js
func ips(c echo.Context) error {
	data, err := loadData(sqlFilter{})
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
	}
	var ips []string
	for _, r := range data {
		ips = append(ips, r.IP)
	}
	return c.JSON(http.StatusOK, ips)
}

type jobTime time.Time

func (jt jobTime) String() string {
	t := time.Time(jt)
	if t.IsZero() {
		return ""
	}
	return t.Format(dateTime)
}

func (jt jobTime) IsZero() bool {
	return time.Time(jt).IsZero()
}

type job struct {
	ID          int     `json:"id"`
	CIDR        string  `json:"cidr"`
	Ports       string  `json:"ports"`
	Proto       string  `json:"proto"`
	RequestedBy string  `json:"-"`
	Submitted   jobTime `json:"-"`
	Received    jobTime `json:"-"`
	Count       int64   `json:"-"`
}

func loadJobs(filter sqlFilter) ([]job, error) {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return []job{}, err
	}
	defer db.Close()

	qry := fmt.Sprintf(`SELECT rowid, cidr, ports, proto, requested_by, submitted, received, count FROM job %s ORDER BY received DESC, submitted, rowid`, filter)
	rows, err := db.Query(qry, filter.Values...)
	if err != nil {
		return []job{}, err
	}

	defer rows.Close()

	var id int
	var cidr, ports, proto, requestedBy string
	var submitted time.Time
	var received NullTime
	var count sql.NullInt64

	var jobs []job

	for rows.Next() {
		err := rows.Scan(&id, &cidr, &ports, &proto, &requestedBy, &submitted, &received, &count)
		if err != nil {
			return []job{}, err
		}

		jobs = append(jobs, job{id, cidr, ports, proto, requestedBy, jobTime(submitted), jobTime(received.Time), count.Int64})
	}

	return jobs, nil
}

func saveJob(cidr, ports, proto, user string) (int64, error) {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	txn, err := db.Begin()
	if err != nil {
		return 0, err
	}

	qry := `INSERT INTO job (cidr, ports, proto, requested_by, submitted) VALUES (?, ?, ?, ?, ?)`
	res, err := txn.Exec(qry, cidr, ports, strings.ToLower(proto), user, time.Now())
	if err != nil {
		txn.Rollback()
		return 0, err
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	err = txn.Commit()
	if err != nil {
		return 0, err
	}

	return id, nil
}

func updateJob(id string, count int64) error {
	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	txn, err := db.Begin()
	if err != nil {
		return err
	}

	qry := `UPDATE job SET received=?, count=? WHERE rowid=?`
	res, err := txn.Exec(qry, time.Now(), count, id)
	rows, _ := res.RowsAffected()
	if err != nil || rows <= 0 {
		txn.Rollback()
		return err
	}

	err = txn.Commit()
	if err != nil {
		return err
	}

	return nil
}

type jobData struct {
	indexData
	JobID []string
	Jobs  []job
}

// Handler for GET and POST /job
func newJob(c echo.Context) error {
	var user string
	if !authDisabled {
		session, err := store.Get(c.Request(), "user")
		if err != nil {
			return c.String(http.StatusInternalServerError, err.Error())
		}
		if _, ok := session.Values["user"]; !ok {
			data := jobData{}
			return c.Render(http.StatusOK, "job", data)
		}
		user = session.Values["user"].(string)
	}

	var jobID []string

	if c.Request().Method == "POST" {
		f, _ := c.FormParams()
		cidr := f.Get("cidr")
		ports := f.Get("ports")
		proto := f["proto"]

		// If we have form parameters, save the data as a new job.
		// Multiple protocols can be submitted. These are saved as separate jobs.
		//
		// TODO(jamesog): Turn the logic around and add a message in the
		// browser if parameters were missing.
		if cidr != "" && ports != "" && len(proto) > 0 {
			// var err error
			for i := range proto {
				id, err := saveJob(cidr, ports, proto[i], user)
				if err != nil {
					return c.String(http.StatusInternalServerError, err.Error())
				}
				jobID = append(jobID, strconv.FormatInt(id, 10))
			}
		}
	}

	jobs, err := loadJobs(sqlFilter{})
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	data := jobData{
		indexData{Authenticated: true, User: user},
		jobID,
		jobs,
	}

	return c.Render(http.StatusOK, "job", data)
}

// Handler for GET /jobs
func jobs(c echo.Context) error {
	jobs, err := loadJobs(sqlFilter{
		Where: []string{"received IS NULL"},
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, err.Error())
	}

	return c.JSON(http.StatusOK, jobs)
}

func saveResults(c echo.Context) (int64, error) {
	res := new([]result)
	err := c.Bind(res)
	if err != nil {
		return 0, err
	}

	count, err := saveData(*res)
	if err != nil {
		return 0, err
	}

	return count, nil
}

// Handler for POST /results
func recvResults(c echo.Context) error {
	_, err := saveResults(c)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	return c.NoContent(http.StatusOK)
}

// Handler for PUT /results/:id
func recvJobResults(c echo.Context) error {
	job := c.Param("id")

	// Check if the job ID is valid
	_, err := loadJobs(sqlFilter{
		Where:  []string{"rowid=?"},
		Values: []interface{}{job},
	})
	if err != nil {
		return c.String(http.StatusBadRequest, err.Error())
	}

	// Insert the results as normal
	count, err := saveResults(c)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	// Update the job
	err = updateJob(job, count)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	return c.NoContent(http.StatusOK)
}

// Handler for POST /traceroute
func recvTraceroute(c echo.Context) error {
	dest := c.FormValue("dest")
	fh, err := c.FormFile("traceroute")
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}
	f, err := fh.Open()
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}
	path, err := ioutil.ReadAll(f)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}
	defer db.Close()

	txn, err := db.Begin()
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	_, err = txn.Exec(`INSERT INTO traceroute (dest, path) VALUES (?, ?)`, dest, path)
	if err != nil {
		txn.Rollback()
		return c.String(http.StatusInternalServerError, err.Error())
	}

	err = txn.Commit()
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	return c.NoContent(http.StatusOK)
}

// Handler for GET /traceroute/:ip
func traceroute(c echo.Context) error {
	ip := c.Param("ip")

	db, err := sql.Open("sqlite3", dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	var path string
	err = db.QueryRow(`SELECT path FROM traceroute WHERE dest = ?`, ip).Scan(&path)
	switch {
	case err == sql.ErrNoRows:
		return c.String(http.StatusNotFound, "Traceroute not found")
	case err != nil:
		return c.String(http.StatusInternalServerError, err.Error())
	}

	return c.String(http.StatusOK, path)
}

func main() {
	flag.BoolVar(&authDisabled, "no-auth", false, "Disable authentication")
	httpAddr := flag.String("http.addr", ":80", "HTTP address:port")
	httpsAddr := flag.String("https.addr", ":443", "HTTPS address:port")
	tls := flag.Bool("tls", false, "Enable AutoTLS")
	tlsHostname := flag.String("tls.hostname", "", "(Optional) Hostname to restrict AutoTLS")
	flag.Parse()

	funcMap := template.FuncMap{
		"join": func(sep string, s []string) string {
			return strings.Join(s, sep)
		},
	}

	t := &Template{
		templates: template.Must(template.New("").
			Funcs(funcMap).
			ParseGlob("views/*.html")),
	}

	e := echo.New()

	if authDisabled {
		color.Println(color.Red("Authentication Disabled"))
	}

	if *tls {
		if *tlsHostname != "" {
			e.AutoTLSManager.HostPolicy = autocert.HostWhitelist(*tlsHostname)
		}
		e.AutoTLSManager.Cache = autocert.DirCache(".cache")
		e.Pre(middleware.HTTPSRedirect())
	}

	e.HideBanner = true
	e.Renderer = t
	e.Use(middleware.Logger())
	e.GET("/", index)
	e.GET("/auth", authHandler)
	e.GET("/login", loginHandler)
	e.GET("/ips.json", ips)
	e.Match([]string{"GET", "POST"}, "/job", newJob)
	e.GET("/jobs", jobs)
	e.POST("/results", recvResults)
	e.PUT("/results/:id", recvJobResults)
	e.Static("/static", "static")
	e.POST("/traceroute", recvTraceroute)
	e.GET("/traceroute/:ip", traceroute)
	e.GET("/metrics", echo.WrapHandler(metrics()))

	if *tls {
		go func() { e.Logger.Fatal(e.Start(*httpAddr)) }()
		e.Logger.Fatal(e.StartAutoTLS(*httpsAddr))
	}
	e.Logger.Fatal(e.Start(*httpAddr))
}
