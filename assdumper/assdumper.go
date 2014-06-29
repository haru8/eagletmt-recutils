package main

import "fmt"
import "io"
import "os"
import "time"

/*
[B10]: ARIB-STD B10
[ISO]: ISO/IEC 13818-1
*/

const TS_PACKET_SIZE = 188

type AnalyzerState struct {
	pmtPids           map[int]bool
	pcrPid            int
	captionPid        int
	currentTimestamp  SystemClock
	clockOffset       int64
	previousSubtitle  string
	previousIsBlank   bool
	previousTimestamp SystemClock
	preludePrinted    bool
}

type SystemClock int64

func main() {
	if len(os.Args) == 1 {
		fmt.Fprintf(os.Stderr, "usage: %s MPEG2-TS-FILE\n", os.Args[0])
		os.Exit(1)
	}
	fin, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := fin.Close(); err != nil {
			panic(err)
		}
	}()

	buf := make([]byte, TS_PACKET_SIZE)
	state := new(AnalyzerState)
	state.pcrPid = -1
	state.captionPid = -1

	for {
		n, err := fin.Read(buf)
		if err != nil && err != io.EOF {
			panic(err)
		}
		if n == 0 {
			break
		}

		analyzePacket(buf, state)
	}
}

func assertSyncByte(packet []byte) {
	if packet[0] != 0x47 {
		panic("sync_byte failed")
	}
}

func analyzePacket(packet []byte, state *AnalyzerState) {
	assertSyncByte(packet)

	payload_unit_start_indicator := (packet[1] & 0x40) != 0
	pid := int(packet[1]&0x1f)<<8 | int(packet[2])
	hasAdaptation := (packet[3] & 0x20) != 0
	hasPayload := (packet[3] & 0x10) != 0
	p := packet[4:]

	if hasAdaptation {
		// [ISO] 2.4.3.4
		// Table 2-6
		adaptation_field_length := p[0]
		p = p[1:]
		pcr_flag := (p[0] & 0x10) != 0
		if pcr_flag && pid == state.pcrPid {
			state.currentTimestamp = extractPcr(p)
		}
		p = p[adaptation_field_length:]
	}

	if hasPayload {
		if pid == 0 {
			if len(state.pmtPids) == 0 {
				state.pmtPids = extractPmtPids(p[1:])
				fmt.Fprintf(os.Stderr, "Found %d pids: %v\n", len(state.pmtPids), state.pmtPids)
			}
		} else if state.pmtPids != nil && state.pmtPids[pid] {
			if state.captionPid == -1 && payload_unit_start_indicator {
				// PMT section
				pcrPid := extractPcrPid(p[1:])
				captionPid := extractCaptionPid(p[1:])
				if captionPid != -1 {
					fmt.Fprintf(os.Stderr, "caption pid = %d, PCR_PID = %d\n", captionPid, pcrPid)
					state.pcrPid = pcrPid
					state.captionPid = captionPid
				}
			}
		} else if pid == 0x0014 {
			// Time Offset Table
			// [B10] 5.2.9
			t := extractJstTime(p[1:])
			if t != 0 {
				state.clockOffset = t*100 - state.currentTimestamp.centitime()
			}
		} else if pid == state.captionPid {
			if payload_unit_start_indicator {
				dumpCaption(p, state)
			}
		}
	}
}

func extractPmtPids(payload []byte) map[int]bool {
	// [ISO] 2.4.4.3
	// Table 2-25
	table_id := payload[0]
	pids := make(map[int]bool)
	if table_id != 0x00 {
		return pids
	}
	section_length := int(payload[1]&0x0F)<<8 | int(payload[2])
	index := 8
	for index < 3+section_length-4 {
		program_number := int(payload[index+0])<<8 | int(payload[index+1])
		if program_number != 0 {
			program_map_PID := int(payload[index+2]&0x1F)<<8 | int(payload[index+3])
			pids[program_map_PID] = true
		}
		index += 4
	}
	return pids
}

func extractPcrPid(payload []byte) int {
	return (int(payload[8]&0x1f) << 8) | int(payload[9])
}

func extractCaptionPid(payload []byte) int {
	// [ISO] 2.4.4.8 Program Map Table
	// Table 2-28
	table_id := payload[0]
	if table_id != 0x02 {
		return -1
	}
	section_length := int(payload[1]&0x0F)<<8 | int(payload[2])
	if section_length >= len(payload) {
		return -1
	}

	program_info_length := int(payload[10]&0x0F)<<8 | int(payload[11])
	index := 12 + program_info_length

	for index < 3+section_length-4 {
		stream_type := payload[index+0]
		ES_info_length := int(payload[index+3]&0xF)<<8 | int(payload[index+4])
		if stream_type == 0x06 {
			elementary_PID := int(payload[index+1]&0x1F)<<8 | int(payload[index+2])
			subIndex := index + 5
			for subIndex < index+ES_info_length {
				// [ISO] 2.6 Program and program element descriptors
				descriptor_tag := payload[subIndex+0]
				descriptor_length := int(payload[subIndex+1])
				if descriptor_tag == 0x52 {
					// [B10] 6.2.16 Stream identifier descriptor
					// 表 6-28
					component_tag := payload[subIndex+2]
					if component_tag == 0x87 {
						return elementary_PID
					}
				}
				subIndex += 2 + descriptor_length
			}
		}
		index += 5 + ES_info_length
	}
	return -1
}

func extractPcr(payload []byte) SystemClock {
	pcr_base := (int64(payload[1]) << 25) |
		(int64(payload[2]) << 17) |
		(int64(payload[3]) << 9) |
		(int64(payload[4]) << 1) |
		(int64(payload[5]&0x80) >> 7)
	pcr_ext := (int64(payload[5] & 0x01)) | int64(payload[6])
	// [ISO] 2.4.2.2
	return SystemClock(pcr_base*300 + pcr_ext)
}

func extractJstTime(payload []byte) int64 {
	if payload[0] != 0x73 {
		return 0
	}

	// [B10] Appendix C
	MJD := (int(payload[3]) << 8) | int(payload[4])
	y := int((float64(MJD) - 15078.2) / 365.25)
	m := int((float64(MJD) - 14956.1 - float64(int(float64(y)*365.25))) / 30.6001)
	k := 0
	if m == 14 || m == 15 {
		k = 1
	}
	year := y + k + 1900
	month := m - 2 - k*12
	day := MJD - 14956 - int(float64(y)*365.25) - int(float64(m)*30.6001)
	hour := decodeBcd(payload[5])
	minute := decodeBcd(payload[6])
	second := decodeBcd(payload[7])

	str := fmt.Sprintf("%d-%02d-%02dT%02d:%02d:%02d+09:00", year, month, day, hour, minute, second)
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

func decodeBcd(n byte) int {
	return (int(n)>>4)*10 + int(n&0x0f)
}

func dumpCaption(payload []byte, state *AnalyzerState) {
	PES_header_data_length := payload[8]
	PES_data_packet_header_length := payload[11+PES_header_data_length] & 0x0F
	p := payload[12+PES_header_data_length+PES_data_packet_header_length:]

	// [B24] Table 9-1 (p184)
	data_group_id := (p[0] & 0xFC) >> 2
	if data_group_id == 0x00 || data_group_id == 0x20 {
		// [B24] Table 9-3 (p186)
		// caption_management_data
		num_languages := p[6]
		p = p[7+num_languages*5:]
	} else {
		// caption_data
		p = p[6:]
	}
	// [B24] Table 9-3 (p186)
	data_unit_loop_length := (int(p[0]) << 16) | (int(p[1]) << 8) | int(p[2])
	index := 0
	for index < data_unit_loop_length {
		q := p[index:]
		data_unit_parameter := q[4]
		data_unit_size := (int(q[5]) << 16) | (int(q[6]) << 8) | int(q[7])
		if data_unit_parameter == 0x20 {
			if len(state.previousSubtitle) != 0 && !(isBlank(state.previousSubtitle) && state.previousIsBlank) {
				prevTimeCenti := state.previousTimestamp.centitime() + state.clockOffset
				curTimeCenti := state.currentTimestamp.centitime() + state.clockOffset
				prevTime := prevTimeCenti / 100
				curTime := curTimeCenti / 100
				prevCenti := prevTimeCenti % 100
				curCenti := curTimeCenti % 100
				prev := time.Unix(prevTime, 0)
				cur := time.Unix(curTime, 0)
				if !state.preludePrinted {
					printPrelude()
					state.preludePrinted = true
				}
				fmt.Printf("Dialogue: 0,%02d:%02d:%02d.%02d,%02d:%02d:%02d.%02d,Default,,,,,,%s\n",
					prev.Hour(), prev.Minute(), prev.Second(), prevCenti,
					cur.Hour(), cur.Minute(), cur.Second(), curCenti,
					state.previousSubtitle)
			}
			state.previousIsBlank = isBlank(state.previousSubtitle)
			state.previousSubtitle = decodeCprofile(q[8:], data_unit_size)
			state.previousTimestamp = state.currentTimestamp
		}
		index += 5 + data_unit_size
	}
}

func isBlank(str string) bool {
	for _, c := range str {
		if c != ' ' {
			return false
		}
	}
	return true
}

func printPrelude() {
	fmt.Println("[Script Info]")
	fmt.Println("ScriptType: v4.00+")
	fmt.Println("Collisions: Normal")
	fmt.Println("ScaledBorderAndShadow: yes")
	fmt.Println("Timer: 100.0000")
	fmt.Println("\n[Events]")
}

func decodeCprofile(str []byte, length int) string {
	return "dummy"
}

const K int64 = 27000000

func (clock SystemClock) centitime() int64 {
	return int64(clock) / (K / 100)
}
