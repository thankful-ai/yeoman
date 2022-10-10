package terrafirma

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
)

// LoadFixture from testdata, outputting the bytes, or panic if anything fails.
// This should only be used in tests. The path should be relative to the
// testdata directory.
func LoadFixture(pth string) []byte {
	byt, err := ioutil.ReadFile(filepath.Join("testdata", pth))
	if err != nil {
		panic(fmt.Errorf("load fixture: %w", err))
	}
	return byt
}
