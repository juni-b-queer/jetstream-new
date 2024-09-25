package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
)

type WantedCollections struct {
	Prefixes  []string
	FullPaths map[string]struct{}
}

type Subscriber struct {
	ws     *websocket.Conn
	realIP string
	lk     sync.Mutex
	seq    int64
	outbox chan *[]byte
	hello  chan struct{}
	id     int64

	// Subscriber options

	// wantedCollections is nil if the subscriber wants all collections
	wantedCollections *WantedCollections
	wantedDids        map[string]struct{}
	cursor            *int64
	compress          bool

	rl *rate.Limiter

	deliveredCounter prometheus.Counter
	bytesCounter     prometheus.Counter
}

// emitToSubscriber sends an event to a subscriber if the subscriber wants the event
// It takes a valuer function to get the event bytes so that the caller can avoid
// unnecessary allocations and/or reading from the playback DB if the subscriber doesn't want the event
func emitToSubscriber(ctx context.Context, log *slog.Logger, sub *Subscriber, timeUS int64, did, collection string, playback bool, getEventBytes func() []byte) error {
	if !sub.WantsCollection(collection) {
		return nil
	}

	if len(sub.wantedDids) > 0 {
		if _, ok := sub.wantedDids[did]; !ok {
			return nil
		}
	}

	// Skip events that are older than the subscriber's last seen event
	if timeUS <= sub.seq {
		return nil
	}

	evtBytes := getEventBytes()
	if playback {
		// Copy the event bytes so the playback iterator can reuse the buffer
		evtBytes = append([]byte{}, evtBytes...)
		select {
		case <-ctx.Done():
			log.Error("failed to send event to subscriber", "error", ctx.Err(), "subscriber", sub.id)
			// If we failed to send to a subscriber, close the connection
			sub.Terminate("error sending event")
			err := sub.ws.Close()
			if err != nil {
				log.Error("failed to close subscriber connection", "error", err)
			}
			return ctx.Err()
		case sub.outbox <- &evtBytes:
			sub.seq = timeUS
			sub.deliveredCounter.Inc()
			sub.bytesCounter.Add(float64(len(evtBytes)))
		}
	} else {
		select {
		case <-ctx.Done():
			log.Error("failed to send event to subscriber", "error", ctx.Err(), "subscriber", sub.id)
			// If we failed to send to a subscriber, close the connection
			sub.Terminate("error sending event")
			err := sub.ws.Close()
			if err != nil {
				log.Error("failed to close subscriber connection", "error", err)
			}
			return ctx.Err()
		case sub.outbox <- &evtBytes:
			sub.seq = timeUS
			sub.deliveredCounter.Inc()
			sub.bytesCounter.Add(float64(len(evtBytes)))
		default:
			// Drop slow subscribers if they're live tailing and fall too far behind
			log.Error("failed to send event to subscriber, dropping", "error", "buffer full", "subscriber", sub.id)
			sub.Terminate("consuming too slowly")
			err := sub.ws.Close()
			if err != nil {
				log.Error("failed to close subscriber connection", "error", err)
			}
		}
	}

	return nil
}

var SubMessageOptionsUpdate = "options_update"

type SubscriberSourcedMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type SubscriberOptionsUpdatePayload struct {
	WantedCollections []string `json:"wantedCollections"`
	WantedDIDs        []string `json:"wantedDids"`
}

type SubscriberOptions struct {
	WantedCollections *WantedCollections
	WantedDIDs        map[string]struct{}
	Compress          bool
	Cursor            *int64
}

// ErrInvalidOptions is returned when the subscriber options are invalid
var ErrInvalidOptions = fmt.Errorf("invalid subscriber options")

func parseSubscriberOptions(ctx context.Context, wantedCollectionsProvided, wantedDidsProvided []string, compress bool, cursor *int64) (*SubscriberOptions, error) {
	ctx, span := tracer.Start(ctx, "parseSubscriberOptions")
	defer span.End()

	var wantedCol *WantedCollections
	if len(wantedCollectionsProvided) > 0 {
		wantedCol = &WantedCollections{
			Prefixes:  []string{},
			FullPaths: make(map[string]struct{}),
		}

		for _, providedCol := range wantedCollectionsProvided {
			if strings.HasSuffix(providedCol, ".*") {
				wantedCol.Prefixes = append(wantedCol.Prefixes, strings.TrimSuffix(providedCol, "*"))
				continue
			}

			col, err := syntax.ParseNSID(providedCol)
			if err != nil {

				return nil, fmt.Errorf("%w: invalid collection: %s", ErrInvalidOptions, providedCol)
			}
			wantedCol.FullPaths[col.String()] = struct{}{}
		}
	}

	didMap := make(map[string]struct{})
	for _, d := range wantedDidsProvided {
		did, err := syntax.ParseDID(d)
		if err != nil {
			return nil, ErrInvalidOptions
		}
		didMap[did.String()] = struct{}{}
	}

	// Reject requests with too many wanted DIDs
	if len(didMap) > 10_000 {
		return nil, fmt.Errorf("%w: too many wanted DIDs", ErrInvalidOptions)
	}

	// Reject requests with too many wanted collections
	if wantedCol != nil && len(wantedCol.Prefixes)+len(wantedCol.FullPaths) > 100 {
		return nil, fmt.Errorf("%w: too many wanted collections", ErrInvalidOptions)
	}

	return &SubscriberOptions{
		WantedCollections: wantedCol,
		WantedDIDs:        didMap,
		Compress:          compress,
		Cursor:            cursor,
	}, nil
}

func (s *Server) AddSubscriber(ws *websocket.Conn, realIP string, opts *SubscriberOptions) (*Subscriber, error) {
	s.lk.Lock()
	defer s.lk.Unlock()

	sub := Subscriber{
		ws:                ws,
		realIP:            realIP,
		outbox:            make(chan *[]byte, 10_000),
		hello:             make(chan struct{}),
		id:                s.nextSub,
		wantedCollections: opts.WantedCollections,
		wantedDids:        opts.WantedDIDs,
		cursor:            opts.Cursor,
		compress:          opts.Compress,
		deliveredCounter:  eventsDelivered.WithLabelValues(realIP),
		bytesCounter:      bytesDelivered.WithLabelValues(realIP),
		rl:                rate.NewLimiter(rate.Limit(s.maxSubRate), int(s.maxSubRate)),
	}

	s.Subscribers[s.nextSub] = &sub
	s.nextSub++

	subscribersConnected.WithLabelValues(realIP).Inc()

	slog.Info("adding subscriber",
		"real_ip", realIP,
		"id", sub.id,
		"wantedCollections", opts.WantedCollections,
		"wantedDids", opts.WantedDIDs,
		"cursor", opts.Cursor,
		"compress", opts.Compress,
	)

	return &sub, nil
}

func (s *Server) RemoveSubscriber(num int64) {
	s.lk.Lock()
	defer s.lk.Unlock()

	slog.Info("removing subscriber", "id", num, "real_ip", s.Subscribers[num].realIP)

	subscribersConnected.WithLabelValues(s.Subscribers[num].realIP).Dec()

	delete(s.Subscribers, num)
}

// WantsCollection returns true if the subscriber wants the given collection
func (sub *Subscriber) WantsCollection(collection string) bool {
	if sub.wantedCollections == nil {
		return true
	}

	// Start with the full paths for fast lookup
	if len(sub.wantedCollections.FullPaths) > 0 {
		if _, match := sub.wantedCollections.FullPaths[collection]; match {
			return true
		}
	}

	// Check the prefixes (shortest first)
	for _, prefix := range sub.wantedCollections.Prefixes {
		if strings.HasPrefix(collection, prefix) {
			return true
		}
	}

	return false
}

func (s *Subscriber) UpdateOptions(opts *SubscriberOptions) {
	s.lk.Lock()
	defer s.lk.Unlock()

	s.wantedCollections = opts.WantedCollections
	s.wantedDids = opts.WantedDIDs
	s.cursor = opts.Cursor
	s.compress = opts.Compress
}

// Terminate sends a close message to the subscriber
func (s *Subscriber) Terminate(reason string) error {
	return s.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason))
}

func (s *Subscriber) SetCursor(cursor *int64) {
	s.lk.Lock()
	defer s.lk.Unlock()

	s.cursor = cursor
}
