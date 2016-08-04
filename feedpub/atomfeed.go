package main

import (
	"database/sql"
	"encoding/xml"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-oci8"
	"golang.org/x/tools/blog/atom"
	"net/http"
	"time"
)

var db *sql.DB

var connectStr = "esusr/password@//localhost:1521/xe.oracle.docker"

func connectToDB(connectStr string) (*sql.DB, error) {
	db, err := sql.Open("oci8", connectStr)
	if err != nil {
		log.Warnf("Error connecting to oracle: %s", err.Error())
		return nil, err
	}

	//Are we really in an ok state for starters?
	err = db.Ping()
	if err != nil {
		log.Infof("Error connecting to oracle: %s", err.Error())
		return nil, err
	}

	return db, nil
}

func lastUpdateForFeed(feedid string) (time.Time, error) {
	var lastUpdate time.Time

	rows, err := db.Query(`select event_time from feed_data where feedid = :1 and rownum = 1 order by id desc`, feedid)
	if err != nil {
		return lastUpdate, err
	}

	defer rows.Close()

	for rows.Next() {
		rows.Scan(&lastUpdate)
	}

	err = rows.Err()

	return lastUpdate, err
}

func feedHandler(rw http.ResponseWriter, req *http.Request) {

	feedid := mux.Vars(req)["feedid"]
	if feedid == "" {
		http.Error(rw, "No feed id in uri", http.StatusInternalServerError)
		return
	}

	log.Infof("processing request for feed %s", feedid)

	//Look up previous
	feedIdFromDB, previousFeed, err := lookupFeed(feedid)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	if feedIdFromDB == "" {
		http.Error(rw, "", http.StatusNotFound)
		return
	}

	log.Infof("previous feed is %s", previousFeed)

	next, feedIdFromDB, err := lookupNext(feedid)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Infof("next feed is %s", next)

	updated, err := lastUpdateForFeed(feedid)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Infof("last update was %s", updated)

	feed := atom.Feed{
		Title:   "Event store feed",
		ID:      feedid,
		Updated: atom.TimeStr(updated.Format(time.RFC3339)),
	}

	self := atom.Link{
		Href: fmt.Sprintf("http://localhost:4000/notifications/%s", feedid),
		Rel:  "self",
	}

	feed.Link = append(feed.Link, self)

	if previousFeed != "" {
		feed.Link = append(feed.Link, atom.Link{
			Href: fmt.Sprintf("http://localhost:4000/notifications/%s", previousFeed),
			Rel:  "previous",
		})
	}

	if next != "" {
		feed.Link = append(feed.Link, atom.Link{
			Href: fmt.Sprintf("http://localhost:4000/notifications/%s", next),
			Rel:  "next",
		})
	}

	out, err := xml.Marshal(&feed)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	rw.Write(out)

}

func currentFeed() (string, error) {
	rows, err := db.Query(`select feedid from feed_state`)
	if err != nil {
		return "", err
	}

	defer rows.Close()

	var feedid string

	for rows.Next() {
		rows.Scan(&feedid)
	}

	err = rows.Err()

	return feedid, err
}

func lookupFeed(id string) (string, string, error) {
	rows, err := db.Query(`select feedid, previous from feeds where feedid = :1`, id)
	if err != nil {
		return "", "", err
	}

	defer rows.Close()

	var feedid string
	var previous string

	for rows.Next() {
		rows.Scan(&feedid, &previous)
	}

	err = rows.Err()

	return feedid, previous, err
}

func lookupNext(id string) (string, string, error) {
	rows, err := db.Query(`select feedid, previous from feeds where previous = :1`, id)
	if err != nil {
		return "", "", err
	}

	defer rows.Close()

	var feedid string
	var previous string

	for rows.Next() {
		rows.Scan(&feedid, &previous)
	}

	err = rows.Err()

	return feedid, previous, err
}

func topHandler(rw http.ResponseWriter, req *http.Request) {

	feedid, err := currentFeed()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	if feedid == "" {
		http.Error(rw, "Nothing to feed yet", http.StatusNoContent)
		return
	}

	feed := atom.Feed{
		Title:   "Event store feed",
		ID:      feedid,
		Updated: atom.TimeStr(time.Now().Truncate(time.Hour).Format(time.RFC3339)),
	}

	self := atom.Link{
		Href: "http://localhost:4000/notifications/recent",
		Rel:  "self",
	}

	via := atom.Link{
		Href: fmt.Sprintf("http://localhost:4000/notifications/%s", feedid),
		Rel:  "via",
	}

	_, previousFeed, err := lookupFeed(feedid)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	previous := atom.Link{
		Href: fmt.Sprintf("http://localhost:4000/notifications/%s", previousFeed),
		Rel:  "previous",
	}

	feed.Link = append(feed.Link, self)
	feed.Link = append(feed.Link, via)
	feed.Link = append(feed.Link, previous)

	out, err := xml.Marshal(&feed)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	rw.Write(out)
}

func main() {
	var err error
	db, err = connectToDB(connectStr)
	if err != nil {
		log.Fatalf("Error connecting to database: %s", err.Error())
	}

	r := mux.NewRouter()
	r.HandleFunc("/notifications/recent", topHandler)
	r.HandleFunc("/notifications/{feedid}", feedHandler)

	err = http.ListenAndServe(":4000", r)
	if err != nil {
		log.Fatal(err.Error())
	}
}