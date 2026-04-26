package main

import (
	"testing"
)

func TestSimple(t *testing.T) {
	o, err := fetchAndConvert("https://note.kaykraft.org/view?id=2242")
	if err != nil {
		t.Error("Error " + err.Error() + "\nOut: " + o + "\n")
	}
	println(o)
}
