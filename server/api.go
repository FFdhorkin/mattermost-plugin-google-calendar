package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/plugin"
	"github.com/mattermost/mattermost-server/v6/shared/mlog"
	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
)

// ServeHTTP allows the plugin to implement the http.Handler interface. Requests destined for the
// /plugins/{id} path will be routed to the plugin.
//
// The Mattermost-User-Id header will be present if (and only if) the request is by an
// authenticated user.
//
// This demo implementation sends back whether or not the plugin hooks are currently enabled. It
// is used by the web app to recover from a network reconnection and synchronize the state of the
// plugin's hooks.
func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	mlog.Debug("received ServeHTTP " + r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch path := r.URL.Path; path {
	case "/oauth/connect":
		mlog.Debug("received /oauth/connect, connecting calendar")
		p.connectCalendar(w, r)
		mlog.Debug("received /oauth/connect, finished connecting calendar")
	case "/oauth/complete":
		mlog.Debug("received /oauth/complete, completing calendar")
		p.completeCalendar(w, r)
		mlog.Debug("received /oauth/complete, finished completing calendar")
	case "/delete":
		mlog.Debug("received /delete, deleting calendar entry")
		p.deleteEvent(w, r)
		mlog.Debug("received /delete, finished deleting calendar entry")
	case "/handleresponse":
		mlog.Debug("received /handleresponse, starting handlerespone")
		p.handleEventResponse(w, r)
		mlog.Debug("received /handleresponse, finished handlerespone")
	case "/watch":
		mlog.Debug("received /watch, watching calendar")
		p.watchCalendar(w, r)
		mlog.Debug("received /watch, finished watching calendar")
	default:
		mlog.Debug("received invalid path")
		http.NotFound(w, r)
	}
}

func (p *Plugin) connectCalendar(w http.ResponseWriter, r *http.Request) {
	autheduserId := r.Header.Get("Mattermost-User-ID")

	if autheduserId == "" {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	state := fmt.Sprintf("%v_%v", model.NewId()[10], autheduserId)

	if err := p.API.KVSet(state, []byte(state)); err != nil {
		http.Error(w, "Failed to save state", http.StatusBadRequest)
		return
	}

	calendarConfig := p.CalendarConfig()

	url := calendarConfig.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (p *Plugin) completeCalendar(w http.ResponseWriter, r *http.Request) {
	html := `
	<!DOCTYPE html>
	<html>
		<head>
			<script>
				window.close();
			</script>
		</head>
		<body>
			<p>Completed connecting to Google Calendar. Please close this window.</p>
		</body>
	</html>
	`
	autheduserId := r.Header.Get("Mattermost-User-ID")
	state := r.FormValue("state")
	code := r.FormValue("code")
	userId := strings.Split(state, "_")[1]
	config := p.CalendarConfig()

	if autheduserId == "" || userId != autheduserId {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}
	mlog.Debug("Got to A")

	storedState, apiErr := p.API.KVGet(state)
	if apiErr != nil {
		http.Error(w, "Missing stored state", http.StatusBadRequest)
		return
	}

	mlog.Debug("Got to B")

	if string(storedState) != state {
		http.Error(w, "Invalid state", http.StatusBadRequest)
		return
	}

	mlog.Debug("Got to C")

	if err := p.API.KVDelete(state); err != nil {
		http.Error(w, "Error deleting state", http.StatusBadRequest)
		return
	}

	mlog.Debug("Got to D")

	token, err := config.Exchange(context.Background(), code)
	if err != nil {
		http.Error(w, "Error setting up Config Exchange", http.StatusBadRequest)
		return
	}

	mlog.Debug("Got to E")

	tokenJSON, err := json.Marshal(token)
	if err != nil {
		http.Error(w, "Invalid token marshal in completeCalendar", http.StatusBadRequest)
		return
	}

	mlog.Debug("Got to F")

	p.API.KVSet(userId+"calendarToken", tokenJSON)

	mlog.Debug("Got to G")

	err = p.CalendarSync(userId)

	mlog.Debug("Got to H")
	if err != nil {
		mlog.Warn("failed sync fresh calender", mlog.String("error", err.Error()))
		p.API.LogWarn("failed sync fresh calender", "error", err.Error())
		http.Error(w, "failed sync fresh calender", http.StatusInternalServerError)
		return
	}

	mlog.Debug("Got to I")

	if err = p.setupCalendarWatch(userId); err != nil {
		mlog.Error("failed setupCalendarwatch", mlog.String("error", err.Error()))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	mlog.Debug("Got to J")

	p.scheduleJob(autheduserId)

	mlog.Debug("Got to K")

	// Post intro post
	message := "#### Welcome to the Mattermost Google Calendar Plugin!\n" +
		"You've successfully connected your Mattermost account to your Google Calendar.\n" +
		"Please type **/calendar help** to understand how to user this plugin. "

	p.CreateBotDMPost(userId, message)

	mlog.Debug("Got to L")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	mlog.Debug("Got to M")
	fmt.Fprint(w, html)

	mlog.Debug("Got to N")
}

func (p *Plugin) deleteEvent(w http.ResponseWriter, r *http.Request) {
	html := `
	<!DOCTYPE html>
	<html>
		<head>
			<script>
				window.close();
			</script>
		</head>
	</html>
	`
	userId := r.Header.Get("Mattermost-User-ID")
	eventID := r.URL.Query().Get("evtid")
	calendarID := p.getPrimaryCalendarID(userId)
	srv, err := p.getCalendarService(userId)
	if err != nil {
		p.CreateBotDMPost(userId, fmt.Sprintf("Unable to delete event. Error: %s", err))
		return
	}

	eventToBeDeleted, _ := srv.Events.Get(calendarID, eventID).Do()
	err = srv.Events.Delete(calendarID, eventID).Do()
	if err != nil {
		p.CreateBotDMPost(userId, fmt.Sprintf("Unable to delete event. Error: %s", err.Error()))
		return
	}

	p.CreateBotDMPost(userId, fmt.Sprintf("Success! Event _%s_ has been deleted.", eventToBeDeleted.Summary))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, html)
}

func (p *Plugin) handleEventResponse(w http.ResponseWriter, r *http.Request) {
	html := `
	<!DOCTYPE html>
	<html>
		<head>
			<script>
				window.close();
			</script>
		</head>
	</html>
	`

	userId := r.Header.Get("Mattermost-User-ID")
	response := r.URL.Query().Get("response")
	eventID := r.URL.Query().Get("evtid")
	calendarID := p.getPrimaryCalendarID(userId)
	srv, _ := p.getCalendarService(userId)

	eventToBeUpdated, err := srv.Events.Get(calendarID, eventID).Do()
	if err != nil {
		p.CreateBotDMPost(userId, fmt.Sprintf("Error! Failed to update the response of _%s_ event.", eventToBeUpdated.Summary))
		return
	}

	for idx, attendee := range eventToBeUpdated.Attendees {
		if attendee.Self {
			eventToBeUpdated.Attendees[idx].ResponseStatus = response
		}
	}

	event, err := srv.Events.Update(calendarID, eventID, eventToBeUpdated).Do()
	if err != nil {
		p.CreateBotDMPost(userId, fmt.Sprintf("Error! Failed to update the response of _%s_ event.", event.Summary))
	} else {
		p.CreateBotDMPost(userId, fmt.Sprintf("Success! Event _%s_ response has been updated.", event.Summary))
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, html)
}

func (p *Plugin) watchCalendar(w http.ResponseWriter, r *http.Request) {
	userId := r.URL.Query().Get("userId")
	channelID := r.Header.Get("X-Goog-Channel-ID")
	resourceID := r.Header.Get("X-Goog-Resource-ID")
	state := r.Header.Get("X-Goog-Resource-State")

	watchToken, _ := p.API.KVGet(userId + "watchToken")
	mlog.Debug("Got to watchCalendar KVGet watchChannel")
	channelByte, _ := p.API.KVGet(userId + "watchChannel")
	var channel calendar.Channel
	json.Unmarshal(channelByte, &channel)
	if string(watchToken) == channelID && state == "exists" {
		p.CalendarSync(userId)
	} else {
		srv, _ := p.getCalendarService(userId)
		srv.Channels.Stop(&calendar.Channel{
			Id:         channelID,
			ResourceId: resourceID,
		})
	}
}
