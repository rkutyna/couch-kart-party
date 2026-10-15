package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
)

// Catalog is web/catalog.json: the characters, vehicles and course cups. The
// server validates lobby choices against it and the phones and TV load the
// same file. Edit the JSON, not the Go code.
type Catalog struct {
	Characters []CatCharacter `json:"characters"`
	Vehicles   []CatVehicle   `json:"vehicles"`
	Cups       []CatCup       `json:"cups"`

	chars    map[string]CatCharacter
	vehicles map[string]CatVehicle
	courses  map[string]bool
}

type CatCharacter struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Weight   string `json:"weight"`
	Unlocked bool   `json:"unlocked"`
}

type CatVehicle struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Weight   string `json:"weight"`
	Type     string `json:"type"`
	Unlocked bool   `json:"unlocked"`
}

type CatCup struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Unlocked bool        `json:"unlocked"`
	Courses  []CatCourse `json:"courses"`
}

type CatCourse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// loadCatalog reads catalog.json from fsys and returns it with a short
// content hash, which the lobby reports as catalogVersion.
func loadCatalog(fsys fs.FS) (*Catalog, string, error) {
	raw, err := fs.ReadFile(fsys, "catalog.json")
	if err != nil {
		return nil, "", err
	}
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, "", fmt.Errorf("catalog.json: %w", err)
	}
	if err := c.index(); err != nil {
		return nil, "", fmt.Errorf("catalog.json: %w", err)
	}
	sum := sha256.Sum256(raw)
	return &c, hex.EncodeToString(sum[:])[:12], nil
}

func (c *Catalog) index() error {
	weights := map[string]bool{"light": true, "medium": true, "heavy": true}
	c.chars = map[string]CatCharacter{}
	for _, ch := range c.Characters {
		if ch.ID == "" || !weights[ch.Weight] {
			return fmt.Errorf("character %q: bad id or weight %q", ch.ID, ch.Weight)
		}
		if _, dup := c.chars[ch.ID]; dup {
			return fmt.Errorf("duplicate character id %q", ch.ID)
		}
		c.chars[ch.ID] = ch
	}
	c.vehicles = map[string]CatVehicle{}
	for _, v := range c.Vehicles {
		if v.ID == "" || !weights[v.Weight] || (v.Type != "kart" && v.Type != "bike") {
			return fmt.Errorf("vehicle %q: bad id, weight or type", v.ID)
		}
		if _, dup := c.vehicles[v.ID]; dup {
			return fmt.Errorf("duplicate vehicle id %q", v.ID)
		}
		c.vehicles[v.ID] = v
	}
	c.courses = map[string]bool{}
	for _, cup := range c.Cups {
		for _, co := range cup.Courses {
			if co.ID == "" || co.ID == "random" || c.courses[co.ID] {
				return fmt.Errorf("course %q: bad or duplicate id", co.ID)
			}
			c.courses[co.ID] = true
		}
	}
	if len(c.chars) == 0 || len(c.vehicles) == 0 || len(c.courses) == 0 {
		return fmt.Errorf("catalog needs characters, vehicles and courses")
	}
	return nil
}

func (c *Catalog) character(id string) (CatCharacter, bool) {
	ch, ok := c.chars[id]
	return ch, ok
}

func (c *Catalog) vehicle(id string) (CatVehicle, bool) {
	v, ok := c.vehicles[id]
	return v, ok
}

func (c *Catalog) validVote(id string) bool {
	return id == "random" || c.courses[id]
}
