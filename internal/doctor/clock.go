package doctor

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// ntpOffset asks an NTP server for the time, as an SNTP client (RFC 4330).
// A positive offset means that this clock is ahead.
func ntpOffset(ctx context.Context, server string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return 0, fmt.Errorf("ask %s for the time: %w", server, err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	req := make([]byte, 48)
	req[0] = 4<<3 | 3 // version 4, client mode
	sent := time.Now()
	if _, err := conn.Write(req); err != nil {
		return 0, fmt.Errorf("ask %s for the time: %w", server, err)
	}
	resp := make([]byte, 48)
	n, err := conn.Read(resp)
	received := time.Now()
	if err != nil {
		return 0, fmt.Errorf("read the time of %s: %w", server, err)
	}
	if n < 48 || resp[0]&7 != 4 || resp[1] == 0 { // server mode; stratum 0 is a refusal
		return 0, fmt.Errorf("read the time of %s: bad answer", server)
	}
	serverReceived, serverSent := ntpTime(resp[32:40]), ntpTime(resp[40:48])
	return -(serverReceived.Sub(sent) + serverSent.Sub(received)) / 2, nil
}

// ntpTime reads a 64-bit NTP timestamp. Seconds without the top bit belong
// to the era that starts in 2036.
func ntpTime(b []byte) time.Time {
	sec := int64(binary.BigEndian.Uint32(b[:4]))
	if sec&0x80000000 == 0 {
		sec += 1 << 32
	}
	frac := int64(binary.BigEndian.Uint32(b[4:]))
	const unixToNTP = 2208988800
	return time.Unix(sec-unixToNTP, frac*1e9>>32)
}
