package randomsanity

import (
	"appengine"
	"appengine/datastore"
	"appengine/mail"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

import netmail "net/mail"

// Code to notify customer when a rng failure is detected

type NotifyViaEmail struct {
	UserID  string
	Address string
}

// Return userID associated with request (or empty string)
func userID(ctx appengine.Context, id string) (*datastore.Key, error) {
	// Only pay attention to ?id=123456 if they've done an authentication loop
	// and are already in the database
	if len(id) == 0 {
		return nil, nil
	}
	q := datastore.NewQuery("NotifyViaEmail").Filter("UserID =", id).Limit(1).KeysOnly()
	keys, err := q.GetAll(ctx, nil)
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	return keys[0], nil
}

// Register an email address. To authenticate ownership of the
// address, the server assigns a random user id and emails it.
// To mitigate abuse, this method is heavily rate-limited per
// IP and email address
func registerEmailHandler(w http.ResponseWriter, r *http.Request) {
	// Requests generated by web browsers are not allowed:
	if r.Header.Get("Origin") != "" {
		http.Error(w, "CORS requests are not allowed", http.StatusForbidden)
		return
	}
	ua := r.Header.Get("User-Agent")
	if len(ua) < 4 || (!strings.EqualFold(ua[0:4], "curl") && !strings.EqualFold(ua[0:4], "wget")) {
		http.Error(w, "Email registration must be done via curl or wget", http.StatusForbidden)
		return
	}

	w.Header().Add("Content-Type", "text/plain")
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "Missing email", http.StatusBadRequest)
		return
	}
	if len(parts) > 4 {
		http.Error(w, "URL path too long", http.StatusBadRequest)
		return
	}

	addresses, err := netmail.ParseAddressList(parts[len(parts)-1])
	if err != nil || len(addresses) != 1 {
		http.Error(w, "Invalid email address", http.StatusBadRequest)
		return
	}
	address := addresses[0]

	ctx := appengine.NewContext(r)

	// 2 registrations per IP per day
	limited, err := RateLimitResponse(ctx, w, IPKey("emailreg", r.RemoteAddr), 2, time.Hour*24)
	if err != nil || limited {
		return
	}
	// ... and 1 per email per week
	limited, err = RateLimitResponse(ctx, w, "emailreg"+address.Address, 1, time.Hour*24*7)
	if err != nil || limited {
		return
	}
	// ... and global 10 signups per hour (so a botnet with lots of IPs cannot
	// generate a huge surge of bogus registrations)
	limited, err = RateLimitResponse(ctx, w, "emailreg", 10, time.Hour)
	if err != nil || limited {
		return
	}
	// Note: the AppEngine dashboard can also be used to set quotas.
	// If somebody with a bunch of IP addresses is persistently annoying,
	// we'll switch to a web page with a CAPTCHA or require sign-in with
	// a Google account to register or require payment to register.

	var notify []NotifyViaEmail
	q := datastore.NewQuery("NotifyViaEmail").Filter("Address =", address.Address)
	if _, err := q.GetAll(ctx, &notify); err != nil {
		http.Error(w, "Datastore error", http.StatusInternalServerError)
		return
	}
	if len(notify) > 0 {
		sendNewID(ctx, address.Address, notify[0].UserID)
		fmt.Fprintf(w, "Check your email, ID sent to %s\n", address.Address)
		return
	}
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		http.Error(w, "rand.Read error", http.StatusInternalServerError)
		return
	}
	id := hex.EncodeToString(bytes)
	n := NotifyViaEmail{id, address.Address}
	k := datastore.NewIncompleteKey(ctx, "NotifyViaEmail", nil)
	if _, err := datastore.Put(ctx, k, &n); err != nil {
		http.Error(w, "Datastore error", http.StatusInternalServerError)
		return
	}
	sendNewID(ctx, address.Address, id)
	// HTTP response MUST NOT contain the id
	fmt.Fprintf(w, "Check your email, ID sent to %s", address.Address)
}

// Unregister, given userID
func unRegisterIDHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		http.Error(w, "unregister method must be DELETE", http.StatusBadRequest)
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "Missing userID", http.StatusBadRequest)
		return
	}
	if len(parts) > 4 {
		http.Error(w, "URL path too long", http.StatusBadRequest)
		return
	}
	ctx := appengine.NewContext(r)

	uID := parts[len(parts)-1]
	dbKey, err := userID(ctx, uID)
	if err != nil {
		http.Error(w, "datastore error", http.StatusInternalServerError)
		return
	}
	if dbKey == nil {
		http.Error(w, "User ID not found", http.StatusNotFound)
		return
	}
	err = datastore.Delete(ctx, dbKey)
	if err != nil {
		http.Error(w, "Error deleting key", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "id %s unregistered\n", uID)
}

func sendNewID(ctx appengine.Context, address string, id string) {
	msg := &mail.Message{
		Sender:  "randomsanityalerts@gmail.com",
		To:      []string{address},
		Subject: "Random Sanity id request",
	}
	msg.Body = fmt.Sprintf("Somebody requested an id for this email address (%s)\n"+
		"for the randomsanity.org service.\n"+
		"\n"+
		"id: %s\n"+
		"\n"+
		"Append ?id=%s to API calls to be notified of failures via email.\n"+
		"\n"+
		"If somebody is pretending to be you and you don't use the randomsanity.org\n"+
		"service, please ignore this message.\n",
		address, id, id)
	if err := mail.Send(ctx, msg); err != nil {
		log.Printf("mail.Send failed: %s", err)
	}
}

func sendEmail(ctx appengine.Context, address string, tag string, b []byte, reason string) {
	// Don't spam if there are hundreds of failures, limit to
	// a handful per day:
	limit, err := RateLimit(ctx, address, 5, time.Hour*24)
	if err != nil || limit {
		return
	}

	msg := &mail.Message{
		Sender:  "randomsanityalerts@gmail.com",
		To:      []string{address},
		Subject: "Random Number Generator Failure Detected",
	}
	msg.Body = fmt.Sprintf("The randomsanity.org service has detected a failure.\n"+
		"\n"+
		"Failure reason: %s\n"+
		"Data: 0x%s\n"+
		"Tag: %s\n", reason, hex.EncodeToString(b), tag)
	if err := mail.Send(ctx, msg); err != nil {
		log.Printf("mail.Send failed: %s", err)
	}
}

func notify(ctx appengine.Context, uid string, tag string, b []byte, reason string) {
	if len(uid) == 0 {
		return
	}
	q := datastore.NewQuery("NotifyViaEmail").Filter("UserID =", uid)
	for t := q.Run(ctx); ; {
		var d NotifyViaEmail
		_, err := t.Next(&d)
		if err == datastore.Done {
			break
		}
		if err != nil {
			log.Printf("Datastore error: %s", err.Error())
			return
		}
		sendEmail(ctx, d.Address, tag, b, reason)
	}
}