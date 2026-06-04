package soul

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
)

// Section names the editable soul list a derived ID belongs to. User
// facts and operating instructions are the two curatable lists; their
// IDs are namespaced so the same text in each never collides.
type Section string

const (
	SectionUserFact    Section = "user_fact"
	SectionInstruction Section = "instruction"
)

// Item is one soul list element exposed to the editor with a stable,
// content-derived ID. The ID survives reordering and unrelated add/
// remove (it is a hash of the section + text, NOT a positional index),
// so an edit-by-ID can't clobber the wrong line after the list shifts.
type Item struct {
	ID   string
	Text string
}

// ErrItemNotFound means no list element in the section matches the ID.
var ErrItemNotFound = errors.New("soul: item not found")

// ErrEmptyItem means an add/edit was handed blank text.
var ErrEmptyItem = errors.New("soul: item text is empty")

// DeriveItemID returns the stable ID for an item with the given text in
// the given section. Pure function of its inputs: same text → same ID,
// so IDs need no storage and no schema change. Exact-duplicate text in a
// section yields the same ID (operations resolve to the first match,
// deterministically) — acceptable since editing one of two identical
// lines is inherently ambiguous.
func DeriveItemID(section Section, text string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(section))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.TrimSpace(text)))
	return fmt.Sprintf("%s-%016x", section, h.Sum64())
}

// sectionItems returns the live slice pointer for a section so the edit
// methods read and mutate the one backing array.
func (s *Soul) sectionItems(section Section) (*[]string, error) {
	switch section {
	case SectionUserFact:
		return &s.User.Facts, nil
	case SectionInstruction:
		return &s.Instructions.Items, nil
	default:
		return nil, fmt.Errorf("soul: unknown section %q", section)
	}
}

// Items returns the section's elements as ID+text pairs (render order).
func (s *Soul) Items(section Section) ([]Item, error) {
	src, err := s.sectionItems(section)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(*src))
	for _, t := range *src {
		out = append(out, Item{ID: DeriveItemID(section, t), Text: t})
	}
	return out, nil
}

// AddItem appends a new element to the section and returns its ID.
func (s *Soul) AddItem(section, text string) (string, error) {
	sec := Section(section)
	src, err := s.sectionItems(sec)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrEmptyItem
	}
	*src = append(*src, text)
	return DeriveItemID(sec, text), nil
}

// EditItem replaces the element identified by id with text, returning the
// new ID (it changes because the ID is content-derived).
func (s *Soul) EditItem(section, id, text string) (string, error) {
	sec := Section(section)
	src, err := s.sectionItems(sec)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrEmptyItem
	}
	i, err := indexOfID(*src, sec, id)
	if err != nil {
		return "", err
	}
	(*src)[i] = text
	return DeriveItemID(sec, text), nil
}

// RemoveItem deletes the element identified by id from the section.
func (s *Soul) RemoveItem(section, id string) error {
	sec := Section(section)
	src, err := s.sectionItems(sec)
	if err != nil {
		return err
	}
	i, err := indexOfID(*src, sec, id)
	if err != nil {
		return err
	}
	*src = append((*src)[:i], (*src)[i+1:]...)
	return nil
}

// indexOfID finds the first element whose derived ID matches.
func indexOfID(items []string, section Section, id string) (int, error) {
	for i, t := range items {
		if DeriveItemID(section, t) == id {
			return i, nil
		}
	}
	return -1, fmt.Errorf("%w: %s/%s", ErrItemNotFound, section, id)
}
