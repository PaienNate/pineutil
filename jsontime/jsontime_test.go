package utils

import (
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"
)

func TestMarshalUnmarshalJSON_NonZero(t *testing.T) {
	src, err := FromString("2020-01-02 03:04:05")
	if err != nil {
		t.Fatalf("FromString error: %v", err)
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if string(b) != `"2020-01-02 03:04:05"` {
		t.Fatalf("unexpected json: %s", string(b))
	}
	var dst Time
	if err := json.Unmarshal(b, &dst); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if !time.Time(src).Equal(time.Time(dst)) {
		t.Fatalf("roundtrip mismatch: %v != %v", src, dst)
	}
}

func TestMarshalUnmarshalJSON_NullAndEmpty(t *testing.T) {
	var zero Time
	b, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if string(b) != "null" {
		t.Fatalf("expected null for zero time, got %s", string(b))
	}

	var t1 Time
	if err := json.Unmarshal([]byte("null"), &t1); err != nil {
		t.Fatalf("Unmarshal null error: %v", err)
	}
	if !time.Time(t1).IsZero() {
		t.Fatalf("expected zero time for null, got %v", t1)
	}

	var t2 Time
	if err := json.Unmarshal([]byte(`""`), &t2); err != nil {
		t.Fatalf("Unmarshal empty string error: %v", err)
	}
	if !time.Time(t2).IsZero() {
		t.Fatalf("expected zero time for empty string, got %v", t2)
	}
}

func TestValueAndScan(t *testing.T) {
	var zero Time
	v, err := zero.Value()
	if err != nil {
		t.Fatalf("Value error: %v", err)
	}
	if v != nil {
		t.Fatalf("expected nil for zero time value, got %v", v)
	}

	src, err := FromString("2020-01-02 03:04:05")
	if err != nil {
		t.Fatalf("FromString error: %v", err)
	}
	v, err = src.Value()
	if err != nil {
		t.Fatalf("Value error: %v", err)
	}
	tv, ok := v.(time.Time)
	if !ok {
		t.Fatalf("expected time.Time from Value, got %T", v)
	}

	var dst Time
	if err := dst.Scan(tv); err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if !time.Time(src).Equal(time.Time(dst)) {
		t.Fatalf("scan mismatch: %v != %v", src, dst)
	}

	var dstZero Time
	if err := dstZero.Scan(nil); err != nil {
		t.Fatalf("Scan nil error: %v", err)
	}
	if !time.Time(dstZero).IsZero() {
		t.Fatalf("expected zero time after Scan(nil), got %v", dstZero)
	}
}

func TestNowAndFactories(t *testing.T) {
	n := Now()
	if time.Time(n).IsZero() {
		t.Fatalf("Now returned zero time")
	}

	base := time.Unix(1600000000, 123456000)
	fromTime := FromTime(base)
	if !time.Time(fromTime).Equal(time.Time(fromTime)) {
		t.Fatalf("FromTime not stable")
	}

	fromUnix := FromUnix(1600000000)
	if time.Time(fromUnix).Unix() != 1600000000 {
		t.Fatalf("FromUnix mismatch: %v", time.Time(fromUnix))
	}

	fromMilli := FromUnixMilli(1600000000123)
	if time.Time(fromMilli).UnixMilli() != 1600000000123 {
		t.Fatalf("FromUnixMilli mismatch: %v", time.Time(fromMilli))
	}

	fromNano := FromUnixNano(1600000000123456000)
	if time.Time(fromNano).UnixNano() != 1600000000123456000 {
		t.Fatalf("FromUnixNano mismatch: %v", time.Time(fromNano))
	}
}

func TestImplementsDriverValuerAndScanner(t *testing.T) {
	var _ driver.Valuer = Time{}
	var _ interface{ Scan(interface{}) error } = &Time{}
}
