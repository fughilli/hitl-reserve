package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/fughilli/hitl-reserve/engine"
	"github.com/fughilli/hitl-reserve/wifi"
)

// wifiRoutes serves the AP's per-station link stats, scoped to one reservation:
//
//	GET  /reservation/{id}/wifi              stats for the stations {id} owns
//	POST /reservation/{id}/wifi {"ip": "…"}  claim the station at ip (exclusive), then
//	                                         return the same report
//
// Only an ACTIVE reservation may read or claim (409 otherwise), and a report only
// ever contains the reservation's own stations: its network components' resolved
// addresses plus IPs it claimed (see package wifi). 501 when the daemon runs with no
// --wifi-iface. The trust model is the reservation id, as for heartbeat/release.
func wifiRoutes(mux *http.ServeMux, mgr *engine.Manager, st *wifi.Stations) {
	report := func(w http.ResponseWriter, r *http.Request, id string) {
		unit, err := mgr.ActiveUnit(id)
		if err != nil {
			writeReservationErr(w, err)
			return
		}
		var addrs []string
		for _, c := range unit.Components {
			if c.Kind == "network" && c.Address != "" {
				addrs = append(addrs, c.Address)
			}
		}
		rep, err := st.Report(r.Context(), id, unit.Name, addrs)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rep)
	}
	isActive := func(id string) bool {
		_, err := mgr.ActiveUnit(id)
		return err == nil
	}

	mux.HandleFunc("GET /reservation/{id}/wifi", func(w http.ResponseWriter, r *http.Request) {
		if st == nil {
			writeErr(w, http.StatusNotImplemented, "station stats disabled (daemon has no --wifi-iface)")
			return
		}
		report(w, r, r.PathValue("id"))
	})

	mux.HandleFunc("POST /reservation/{id}/wifi", func(w http.ResponseWriter, r *http.Request) {
		if st == nil {
			writeErr(w, http.StatusNotImplemented, "station stats disabled (daemon has no --wifi-iface)")
			return
		}
		id := r.PathValue("id")
		var body struct {
			IP string `json:"ip"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.IP == "" {
			writeErr(w, http.StatusBadRequest, `body must be {"ip": "<station ip>"}`)
			return
		}
		if _, err := mgr.ActiveUnit(id); err != nil {
			writeReservationErr(w, err)
			return
		}
		if err := st.Claim(id, body.IP, isActive); err != nil {
			switch {
			case errors.Is(err, wifi.ErrClaimed):
				writeErr(w, http.StatusConflict, err.Error())
			case errors.Is(err, wifi.ErrNoStation):
				writeErr(w, http.StatusNotFound, err.Error())
			default:
				writeErr(w, http.StatusBadRequest, err.Error())
			}
			return
		}
		report(w, r, id)
	})
}

func writeReservationErr(w http.ResponseWriter, err error) {
	if errors.Is(err, engine.ErrNotActive) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeErr(w, http.StatusNotFound, err.Error())
}
