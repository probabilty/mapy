package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/nats-io/nats.go"
	typesense "github.com/typesense/typesense-go/typesense"
)

// Parent holds minimal parent polygon info
type Parent struct {
	ID       int64             `json:"id"`
	Names    map[string]string `json:"names"`
	Admin    string            `json:"admin_level"`
	Geometry string            `json:"geometry"`
}

// EnrichedFeature as published by enricher service
type EnrichedFeature struct {
	ID       int64             `json:"id"`
	Type     string            `json:"type"`     // POINT, LINESTRING, POLYGON
	Geometry string            `json:"geometry"` // WKT
	Tags     map[string]string `json:"tags"`     // original OSM tags
	Names    map[string]string `json:"names"`    // default + localized names
	Street   map[string]string `json:"street"`   // localized street names for points
	Parents  []Parent          `json:"parents"`  // sorted by area
	Wiki     string            `json:"wiki,omitempty"`
	Contact  map[string]string `json:"contact,omitempty"`
}

func main() {
	// CLI flags
	natsURL := flag.String("nats", "nats://nats:4222", "NATS server URL")
	esURL := flag.String("es", "http://elasticsearch:9200", "Elasticsearch URL")
	tsURL := flag.String("ts", "http://typesense:8108", "Typesense server URL")
	tsKey := flag.String("ts-key", "xyz", "Typesense API key")
	subj := flag.String("subject", "osm.place.enriched", "NATS subject for enriched features")
	flag.Parse()

	// Connect Elasticsearch
	es, err := elasticsearch.NewClient(elasticsearch.Config{Addresses: []string{*esURL}})
	if err != nil {
		log.Fatalf("Elasticsearch connect error: %v", err)
	}

	// Connect Typesense
	ts := typesense.NewClient(
		typesense.WithServer(*tsURL),
		typesense.WithAPIKey(*tsKey),
	)

	// Connect NATS
	nc, err := nats.Connect(*natsURL)
	if err != nil {
		log.Fatalf("NATS connect error: %v", err)
	}
	defer nc.Drain()

	js, err := nc.JetStream()
	if err != nil {
		log.Fatalf("JetStream context error: %v", err)
	}

	// Ensure stream exists
	_, err = js.AddStream(&nats.StreamConfig{
		Name:     "OSM_ENRICH_STREAM",
		Subjects: []string{*subj},
		Storage:  nats.FileStorage,
	})
	if err != nil && !strings.Contains(err.Error(), "stream name already in use") {
		log.Fatalf("AddStream error: %v", err)
	}

	// Subscribe to enriched features
	sub, err := js.Subscribe(*subj, func(msg *nats.Msg) {
		var feat EnrichedFeature
		if err := json.Unmarshal(msg.Data, &feat); err != nil {
			log.Printf("Invalid enriched feature: %v", err)
			msg.Ack()
			return
		}

		// Build address strings per language
		addresses := make(map[string]string)
		// collect all languages from Names and Parents
		langs := map[string]struct{}{}
		for lang := range feat.Names {
			langs[lang] = struct{}{}
		}
		for _, p := range feat.Parents {
			for lang := range p.Names {
				langs[lang] = struct{}{}
			}
		}
		for lang := range langs {
			parts := []string{}
			// parent addresses
			for _, p := range feat.Parents {
				name := p.Names[lang]
				if name == "" {
					name = p.Names["default"]
				}
				parts = append(parts, name)
			}
			// street for points
			if feat.Type == "POINT" {
				street := feat.Street[lang]
				if street == "" {
					street = feat.Street["default"]
				}
				if street != "" {
					parts = append(parts, street)
				}
			}
			addresses[lang] = strings.Join(parts, ", ")
		}

		// Determine place type
		placeType := ""
		if v, ok := feat.Tags["amenity"]; ok {
			placeType = v
		} else if v, ok := feat.Tags["highway"]; ok {
			placeType = v
		} else if v, ok := feat.Tags["building"]; ok {
			placeType = v
		}

		// Build index document
		doc := map[string]interface{}{
			"id":         feat.ID,
			"type":       feat.Type,
			"geometry":   feat.Geometry,
			"names":      feat.Names,
			"addresses":  addresses,
			"place_type": placeType,
			"wiki":       feat.Wiki,
			"contact":    feat.Contact,
		}

		// Marshal document
		body, err := json.Marshal(doc)
		if err != nil {
			log.Printf("JSON marshal error: %v", err)
			msg.Ack()
			return
		}

		// Index into Elasticsearch
		req := esapi.IndexRequest{
			Index:      "places",
			DocumentID: strconv.FormatInt(feat.ID, 10),
			Body:       strings.NewReader(string(body)),
			Refresh:    "true",
		}
		res, err := req.Do(context.Background(), es)
		if err != nil {
			log.Printf("ES index error: %v", err)
		} else {
			res.Body.Close()
		}

		// Upsert into Typesense
		if _, err := ts.Collection("places").Documents().Upsert(doc).Do(); err != nil {
			log.Printf("TS upsert error: %v", err)
		}

		msg.Ack()
	}, nats.Durable("indexer"), nats.ManualAck())
	if err != nil {
		log.Fatalf("Subscription error: %v", err)
	}
	defer sub.Unsubscribe()

	log.Println("Indexer started, awaiting enriched features...")

	// Wait for termination
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down indexer...")
}
