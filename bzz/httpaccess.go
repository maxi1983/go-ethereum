/*
A simple http server interface to Swarm
*/
package bzz

import (
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/ethutil"
	"io"
	"net/http"
	"regexp"
	"time"
)

const (
	port          = ":8500"
	manifest_type = "application/bzz-manifest+json"
)

var (
	uriMatcher      = regexp.MustCompile("^/raw/[0-9A-Fa-f]{64}$")
	manifestMatcher = regexp.MustCompile("^/[0-9A-Fa-f]{64}")
	hashMatcher     = regexp.MustCompile("^[0-9A-Fa-f]{64}$")
)

type sequentialReader struct {
	reader io.Reader
	pos    int64
	ahead  map[int64](chan bool)
}

type manifestEntry struct {
	Path         string
	Hash         string
	Content_type string
	Status       int16
}

func (self *sequentialReader) ReadAt(target []byte, off int64) (n int, err error) {
	if self.pos != off {
		dpaLogger.Debugf("Swarm: deferred read in POST at position %d, offset %d.",
			self.pos, off)
		wait := make(chan bool)
		self.ahead[off] = wait
		if <-wait {
			// failed read behind
			n = 0
			err = io.ErrUnexpectedEOF
			return
		}
	}
	localPos := 0
	for localPos < len(target) {
		n, err = self.reader.Read(target[localPos:])
		localPos += n
		dpaLogger.Debugf("Swarm: Read %d bytes into buffer size %d from POST, error %v.",
			n, len(target), err)
		if err != nil {
			dpaLogger.Debugf("Swarm: POST stream's reading terminated with %v.", err)
			for i := range self.ahead {
				self.ahead[i] <- true
				self.ahead[i] = nil
			}
			return
		}
		self.pos += int64(n)
	}
	wait := self.ahead[self.pos]
	if wait != nil {
		dpaLogger.Debugf("Swarm: deferred read in POST at position %d triggered.",
			self.pos)
		self.ahead[self.pos] = nil
		close(wait)
	}
	return
}

func handler(w http.ResponseWriter, r *http.Request, dpa *DPA) {
	uri := r.RequestURI
	switch {
	case r.Method == "POST":
		if uri == "/raw" {
			dpaLogger.Debugf("Swarm: POST request received.")
			key, err := dpa.Store(io.NewSectionReader(&sequentialReader{
				reader: r.Body,
				ahead:  make(map[int64]chan bool),
			}, 0, r.ContentLength))
			if err == nil {
				fmt.Fprintf(w, "%064x", key)
				dpaLogger.Debugf("Swarm: Object %064x stored", key)
			} else {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
		} else {
			http.Error(w, "No POST to "+uri+" allowed.", http.StatusBadRequest)
		}
	case r.Method == "GET":
		if uriMatcher.MatchString(uri) {
			dpaLogger.Debugf("Swarm: Raw GET request %s received", uri)
			name := uri[5:]
			key := ethutil.Hex2Bytes(name)
			reader := dpa.Retrieve(key)
			dpaLogger.Debugf("Swarm: Reading %d bytes.", reader.Size())
			http.ServeContent(w, r, name+".bin", time.Unix(0, 0), reader)
			dpaLogger.Debugf("Swarm: Object %s returned.", name)
		} else if manifestMatcher.MatchString(uri) {
			dpaLogger.Debugf("Swarm: Structured GET request %s received.", uri)
			name := uri[1:65]
			path := uri[65:] // typically begins with a /
			key := ethutil.Hex2Bytes(name)
		MANIFEST_RESOLUTION:
			for {
				manifestReader := dpa.Retrieve(key)
				// TODO check size for oversized manifests
				manifest := make([]byte, manifestReader.Size())
				_, err := manifestReader.Read(manifest)
				if err != nil {
					dpaLogger.Debugf("Swarm: Manifest %s not found.", name)
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				dpaLogger.Debugf("Swarm: Manifest %s retrieved.")
				manifestEntries := make([]manifestEntry, 0)
				err = json.Unmarshal(manifest, &manifestEntries)
				if err != nil {
					dpaLogger.Debugf("Swarm: Manifest %s is malformed.", name)
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				} else {
					dpaLogger.Debugf("Swarm: Manifest %s has %d entries.", name, len(manifestEntries))
				}
				var mimeType string
				key = nil
				prefix := 0
				status := int16(404)
			MANIFEST_ENTRIES:
				for _, entry := range manifestEntries {
					if !hashMatcher.MatchString(entry.Hash) {
						// hash is mandatory
						break MANIFEST_ENTRIES
					}
					if entry.Content_type == "" {
						// content type defaults to manifest
						entry.Content_type = manifest_type
					}
					if entry.Status == 0 {
						// status defaults to 200
						entry.Status = 200
					}
					pathLen := len(entry.Path)
					if len(path) >= pathLen && path[:pathLen] == entry.Path && prefix < pathLen {
						prefix = pathLen
						key = ethutil.Hex2Bytes(entry.Hash)
						mimeType = entry.Content_type
						status = entry.Status
					}
				}
				if key == nil {
					http.Error(w, "Object "+uri+" not found.", http.StatusNotFound)
					break MANIFEST_RESOLUTION
				} else if mimeType != manifest_type {
					w.Header().Set("Content-Type", mimeType)
					w.WriteHeader(int(status))
					http.ServeContent(w, r, "", time.Unix(0, 0), dpa.Retrieve(key))
					dpaLogger.Debugf("Swarm: Served %s as %s.", mimeType, uri)
					break MANIFEST_RESOLUTION
				} else {
					path = path[prefix:]
					// continue with manifest resolution
				}
			}
		} else {
			http.Error(w, "Object "+uri+" not found.", http.StatusNotFound)
		}
	default:
		http.Error(w, "Method "+r.Method+" is not supported.", http.StatusMethodNotAllowed)
	}
}

func StartHttpServer(dpa *DPA) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, dpa)
	})
	http.ListenAndServe(port, nil)
}
