package hop

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
)

var idEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

func newID(prefix string) string {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		panic(fmt.Sprintf("generate id: %v", err))
	}
	stamp := uint64(time.Now().UTC().UnixMilli())
	var raw [14]byte
	raw[0] = byte(stamp >> 40)
	raw[1] = byte(stamp >> 32)
	raw[2] = byte(stamp >> 24)
	raw[3] = byte(stamp >> 16)
	raw[4] = byte(stamp >> 8)
	raw[5] = byte(stamp)
	copy(raw[6:], entropy[:])
	return strings.ToUpper(prefix) + "_" + idEncoding.EncodeToString(raw[:])
}
