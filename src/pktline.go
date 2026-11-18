package main

import (
	"bytes"
	"fmt"
	"strings"
)

// pkt-line framing (git's wire format): 4 hex length bytes (payload + 4),
// "0000" flush, "0001" delim, "0002" response-end. Max packet 65520 bytes.

const pktMax = 65520

func pktLine(payload []byte) []byte {
	out := make([]byte, 4, 4+len(payload))
	hexLen := fmt.Sprintf("%04x", len(payload)+4)
	copy(out, hexLen)
	return append(out, payload...)
}

func pktString(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktLines encodes a sequence of payload strings plus flush/delim markers.
// A "" item is a flush; "\x01" is a delim.
func pktLines(items ...string) []byte {
	var b bytes.Buffer
	for _, it := range items {
		switch it {
		case "":
			b.Write(pktFlush)
		case "\x01":
			b.Write(pktDelim)
		default:
			b.Write(pktLine([]byte(it)))
		}
	}
	return b.Bytes()
}

// pktPacket is one decoded pkt-line.
type pktPacket struct {
	kind    byte // 'd' data, 'f' flush, 'e' delim, 'r' response-end
	payload []byte
}

// decodePktLines parses a whole buffer into packets. It stops at the first
// non-pkt-line byte (raw trailing data, e.g. a pack after the flush) and
// returns the offset where parsing stopped.
func decodePktLines(buf []byte) (packets []pktPacket, rest []byte, err error) {
	off := 0
	for off < len(buf) {
		if len(buf)-off < 4 {
			return nil, nil, fmt.Errorf("truncated pkt-line header")
		}
		hdr := string(buf[off : off+4])
		var n int
		if _, err := fmt.Sscanf(hdr, "%04x", &n); err != nil || !isHex4(hdr) {
			// Raw trailing bytes (a pack): stop here.
			return packets, buf[off:], nil
		}
		switch n {
		case 0:
			packets = append(packets, pktPacket{kind: 'f'})
			off += 4
		case 1:
			packets = append(packets, pktPacket{kind: 'e'})
			off += 4
		case 2:
			packets = append(packets, pktPacket{kind: 'r'})
			off += 4
		case 3:
			return nil, nil, fmt.Errorf("invalid pkt-line length 0003")
		default:
			if n > pktMax {
				return nil, nil, fmt.Errorf("pkt-line length %d over %d", n, pktMax)
			}
			if off+n > len(buf) {
				return nil, nil, fmt.Errorf("truncated pkt-line")
			}
			packets = append(packets, pktPacket{kind: 'd', payload: buf[off+4 : off+n]})
			off += n
		}
	}
	return packets, buf[off:], nil
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// sidebandChunks wraps data into band-1 sideband packets (max pktMax each).
func sidebandChunks(band byte, data []byte) []byte {
	var b bytes.Buffer
	const maxPayload = pktMax - 4 - 1 // header + band byte
	for len(data) > 0 {
		n := len(data)
		if n > maxPayload {
			n = maxPayload
		}
		chunk := data[:n]
		data = data[n:]
		payload := make([]byte, 1, 1+len(chunk))
		payload[0] = band
		payload = append(payload, chunk...)
		b.Write(pktLine(payload))
	}
	return b.Bytes()
}

// sidebandText wraps a short progress message as one band-2 packet.
func sidebandText(band byte, text string) []byte {
	payload := append([]byte{band}, text...)
	return pktLine(payload)
}

// packetLines returns the payload strings of data packets.
func packetLines(packets []pktPacket) []string {
	var out []string
	for _, p := range packets {
		if p.kind == 'd' {
			out = append(out, string(p.payload))
		}
	}
	return out
}

// trimNL strips one trailing newline.
func trimNL(s string) string { return strings.TrimSuffix(s, "\n") }
