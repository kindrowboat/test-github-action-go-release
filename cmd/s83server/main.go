// https://github.com/robinsloan/spring-83-spec/blob/main/draft-20220609.md
// TODO:
//  * wipe expired posts
//  * check that the body contains a proper last-modified tag
//  * implement peer sharing and receiving
//  * add /<key> to show a single board
//  * display each board in a region with an aspect ratio of either 1:sqrt(2) or sqrt(2):1
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const MAX_SIG = (1 << 256) - 1

const PAGE_TEMPLATE = `
<!DOCTYPE html>
<html>
<head>
<title>Spring83</title>
<base target="_blank"/>
<style>
	body {
		background-color: lightyellow;
	}
	.board {
		background-color: lightcyan;
		border: 1px dotted black;
		margin: 5px;
		padding: 10px;
		display: inline-flex;
	}
	iframe {
		border: 0;
		height: 300px;
		width: 300px;
	}
</style>
</head>
<body>
<h1>Spring 83</h1>
<div id="containers">
	{{ range .Boards }}
		<div id="b{{ .Key }}" class="board">
			<iframe sandbox="allow-popups allow-popups-to-escape-sandbox" srcdoc="{{ .Board | html }}"></iframe>
		</div>
	{{ end }}
</div>
</body>
</html>
`

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func initDB() *sql.DB {
	dbName := "./spring83.db"

	// if the db doesn't exist, create it
	if _, err := os.Stat(dbName); errors.Is(err, os.ErrNotExist) {
		log.Printf("initializing new database")
		db, err := sql.Open("sqlite3", dbName)
		must(err)

		initSQL := `
		CREATE TABLE boards (
			key text NOT NULL PRIMARY KEY,
			board text,
			expiry text
		);
		`

		_, err = db.Exec(initSQL)
		if err != nil {
			log.Fatal("%q: %s\n", err, initSQL)
		}
		return db
	}

	db, err := sql.Open("sqlite3", dbName)
	must(err)
	return db
}

func main() {
	db := initDB()

	server := newSpring83Server(db)
	http.HandleFunc("/", server.RootHandler)

	port, err := strconv.ParseUint(os.Getenv("PORT"), 10, 16)
	if err != nil {
		port = 8000
	}
	listenAddress := fmt.Sprintf(":%d", port)
	log.Printf("Listening on port %d", port)
	log.Fatal(http.ListenAndServe(listenAddress, nil))
}

func mustTemplate(name string) *template.Template {
	f := PAGE_TEMPLATE

	t, err := template.New("index").Parse(f)
	if err != nil {
		panic(err)
	}

	return t
}

type Spring83Server struct {
	db           *sql.DB
	homeTemplate *template.Template
}

func newSpring83Server(db *sql.DB) *Spring83Server {
	return &Spring83Server{
		db:           db,
		homeTemplate: mustTemplate("server/templates/index.html"),
	}
}

func (s *Spring83Server) getBoard(key string) (*Board, error) {
	query := `
		SELECT key, board, expiry
		FROM boards
		WHERE key=?
	`
	row := s.db.QueryRow(query, key)

	var dbkey, board, expiry string
	err := row.Scan(&dbkey, &board, &expiry)
	if err != nil {
		if err != sql.ErrNoRows {
			return nil, err
		}
		return nil, nil
	}

	expTime, err := time.Parse(time.RFC3339, expiry)
	if err != nil {
		return nil, err
	}

	return &Board{
		Key:    key,
		Board:  board,
		Expiry: expTime,
	}, nil
}

func (s *Spring83Server) boardCount() (int, error) {
	query := `
		SELECT count(*)
		FROM boards
	`
	row := s.db.QueryRow(query)

	var count int
	err := row.Scan(&count)
	if err != nil {
		if err != sql.ErrNoRows {
			return 0, err
		}
		panic(err)
	}

	return count, nil
}

func (s *Spring83Server) getDifficulty() (float64, uint64, error) {
	count, err := s.boardCount()
	if err != nil {
		return 0, 0, err
	}

	difficultyFactor := math.Pow(float64(count)/10_000_000, 4)
	keyThreshold := uint64(MAX_SIG * (1.0 - difficultyFactor))
	return difficultyFactor, keyThreshold, nil
}

func (s *Spring83Server) publishBoard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Spring-Version", "83")

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}

	if len(body) > 2217 {
		http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	//TODO: this time should also be checked againt the <meta http-equiv="last-modified" content="%s"> tag
	//      in fact per the spec, the header is just for convenience/fast failing
	var mtime time.Time
	if ifUnmodifiedHeader, ok := r.Header["If-Unmodified-Since"]; !ok {
		http.Error(w, "Missing If-Unmodified-Since header", http.StatusBadRequest)
		return
	} else {
		// spec says "in HTTP format", but it's not entirely clear if this matches?
		if mtime, err = time.Parse(time.RFC1123, ifUnmodifiedHeader[0]); err != nil {
			http.Error(w, "Invalid format for If-Unmodified-Since header", http.StatusBadRequest)
			return
		}
	}

	key, err := hex.DecodeString(r.URL.Path[1:])
	if err != nil || len(key) != 32 {
		http.Error(w, "Invalid key", http.StatusBadRequest)
		return
	}
	keyStr := fmt.Sprintf("%x", key)

	// curBoard is nil if there is no existing board for this key, and a Board object otherwise
	curBoard, err := s.getBoard(keyStr)
	if err != nil {
		log.Printf(err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if curBoard != nil && !mtime.Before(curBoard.Expiry) {
		http.Error(w, "Old content", http.StatusConflict)
		return
	}

	// if the server doesn't have any board stored for <key>, then it must
	// apply another check. The key, interpreted as a 256-bit number, must be
	// less than a threshold defined by the server's difficulty factor:
	if curBoard == nil {
		difficultyFactor, keyThreshold, err := s.getDifficulty()
		if err != nil {
			log.Printf(err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Add("Spring-Difficulty", fmt.Sprintf("%f", difficultyFactor))

		// Using that difficulty factor, we can calculate the key threshold:
		//
		// MAX_KEY = (2**256 - 1)
		// key_threshold = MAX_KEY * (1.0 - 0.52) = <an inscrutable gigantic number>
		//
		// The server must reject PUT requests for new keys that are not less
		// than <an inscrutable gigantic number>
		if binary.BigEndian.Uint64(key) >= keyThreshold {
			if err != nil || len(key) != 32 {
				http.Error(w, "Key greater than threshold", http.StatusForbidden)
				return
			}
		}
	}

	var signature []byte
	if authorizationHeaders, ok := r.Header["Authorization"]; !ok {
		http.Error(w, "Missing Authorization header", http.StatusBadRequest)
		return
	} else {
		parts := strings.Split(authorizationHeaders[0], " ")
		if parts[0] != "Spring-83" || len(parts) < 2 {
			http.Error(w, "Invalid Authorization Type", http.StatusBadRequest)
			return
		}

		sig := strings.Split(parts[1], "=")
		if len(sig) < 1 {
			http.Error(w, "Invalid Signature", http.StatusBadRequest)
			return
		}

		sigString := sig[1]
		if len(sigString) != 128 {
			http.Error(w, fmt.Sprintf("Expecting 64-bit signature %s %d", sigString, len(sigString)), http.StatusBadRequest)
			return
		}

		signature, err = hex.DecodeString(sigString)
		if err != nil {
			http.Error(w, "Unable to decode signature", http.StatusBadRequest)
			return
		}
	}

	// Spring '83 specifies a test keypair
	// Servers must not accept PUTs for this key, returning 401 Unauthorized.
	// The server may also use a denylist to block certain keys, rejecting all PUTs for those keys.
	denylist := []string{"fad415fbaa0339c4fd372d8287e50f67905321ccfd9c43fa4c20ac40afed1983"}
	for _, key := range denylist {
		if bytes.Compare(signature, []byte(key)) == 0 {
			http.Error(w, "Denied", http.StatusUnauthorized)
		}
	}

	// Keys are of the form 83eMMYY
	// when PUTting, a key must
	// - be greater than today (more specifically the today must be before the first day of the next month following the expir, similar to credit cards)
	// - be less than two years from now
	// The server must reject other keys with 400 Bad Request.
	last4 := string(keyStr[60:64])
	today := time.Now()
	expiry, err := time.Parse("0206", last4)
	if keyStr[57:60] != "83e" || err != nil {
		http.Error(w, "Signature must end with 83eMMYY. You might be using an old key format. Delete your old key, update your client, and try again.", http.StatusBadRequest)
		return
	}
	if today.After(expiry.AddDate(0, 1, 0)) {
		http.Error(w, "Key has expired", http.StatusBadRequest)
		return
	}
	if expiry.After(today.AddDate(2, 0, 0)) {
		http.Error(w, "Key is set to expire more than two years in the future", http.StatusBadRequest)
		return
	}

	// at this point, we should have met all the preconditions prior to the
	// cryptographic check. By the spec, we should perform all
	// non-cryptographic checks first.
	if !ed25519.Verify(key, body, signature) {
		http.Error(w, "Invalid signature", http.StatusBadRequest)
		return
	}

	// TODO: make the "freshness" date modifiable
	boardExpiry := time.Now().AddDate(0, 0, 7).Format(time.RFC3339)
	_, err = s.db.Exec(`
		INSERT INTO boards (key, board, expiry)
		            values(?, ?, ?)
	    ON CONFLICT(key) DO UPDATE SET
			board=?,
			expiry=?
	`, keyStr, body, boardExpiry, body, boardExpiry)

	if err != nil {
		log.Printf("%s", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
	}
}

type Board struct {
	Key    string
	Board  string
	Expiry time.Time
}

func (s *Spring83Server) loadBoards() ([]Board, error) {
	query := `
		SELECT key, board, expiry
		FROM boards
	`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}

	boards := []Board{}
	for rows.Next() {
		var key, board, expiry string

		err = rows.Scan(&key, &board, &expiry)
		if err != nil {
			return nil, err
		}
		// HACK: find a better way to make links open in new windows
		aTagStart := regexp.MustCompile("< *a ")
		board = aTagStart.ReplaceAllString(board, "<a target='_blank'")

		expTime, err := time.Parse(time.RFC3339, expiry)
		if err != nil {
			return nil, err
		}

		boards = append(boards, Board{
			Key:    key,
			Board:  board,
			Expiry: expTime,
		})
	}

	return boards, nil
}

func randstr() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("failed to read random bytes to create random string")
	}

	// format it in hexadecimal, and start it with an n because html can have
	// problems with strings starting with 0 and we're using it as a nonce
	return fmt.Sprintf("n%x", buf)
}

// for now, on loads to /, I'm just going to show all boards no matter what
func (s *Spring83Server) showAllBoards(w http.ResponseWriter, r *http.Request) {
	boards, err := s.loadBoards()
	if err != nil {
		log.Printf(err.Error())
		http.Error(w, "Unable to load boards", http.StatusInternalServerError)
		return
	}

	difficultyFactor, _, err := s.getDifficulty()
	if err != nil {
		log.Printf(err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Add("Spring-Difficulty", fmt.Sprintf("%f", difficultyFactor))

	// XXX: we want to block all javascript from executing, except for our own
	// script, with a CSP but I'm not sure exactly how to do that. This does
	// seem to block a simple onclick handler I added to the code, which is
	// nice
	nonce := randstr()
	w.Header().Add("Content-Security-Policy", fmt.Sprintf("script-src 'nonce-%s'; img-src 'self'", nonce))

	data := struct {
		Boards []Board
		Nonce  string
	}{
		Boards: boards,
		Nonce:  nonce,
	}

	s.homeTemplate.Execute(w, data)
}

func (s *Spring83Server) showBoard(w http.ResponseWriter, r *http.Request) {
	board, err := s.getBoard(r.URL.Path[1:])
	if err != nil {
		log.Printf(err.Error())
		http.Error(w, "Unable to load boards", http.StatusInternalServerError)
		return
	}
	if board == nil {
		http.Error(
			w,
			fmt.Sprintf("Could not find board %s", r.URL.Path[1:]),
			http.StatusNotFound)
		return
	}

	difficultyFactor, _, err := s.getDifficulty()
	if err != nil {
		log.Printf(err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Add("Spring-Difficulty", fmt.Sprintf("%f", difficultyFactor))

	// XXX: we want to block all javascript from executing, except for our own
	// script, with a CSP but I'm not sure exactly how to do that. This does
	// seem to block a simple onclick handler I added to the code, which is
	// nice
	nonce := randstr()
	w.Header().Add("Content-Security-Policy", fmt.Sprintf("script-src 'nonce-%s'; img-src 'self'", nonce))

	boardBytes, err := json.Marshal([]*Board{board})
	if err != nil {
		log.Printf(err.Error())
		http.Error(w, "Unable to marshal boards", http.StatusInternalServerError)
		return
	}

	data := struct {
		Boards string
		Nonce  string
	}{
		Boards: string(boardBytes),
		Nonce:  nonce,
	}

	// for now just be lazy and don't give this page its own template, re-use
	// the page designed to show all boards
	s.homeTemplate.Execute(w, data)
}

func (s *Spring83Server) RootHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		s.publishBoard(w, r)
	} else if r.Method == "GET" {
		if len(r.URL.Path) == 1 {
			s.showAllBoards(w, r)
		} else {
			s.showBoard(w, r)
		}
	} else {
		http.Error(w, "Invalid method", http.StatusBadRequest)
	}
}