package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

const (
	DM_EPOCH = 1356998400 // Digital Matter epoch: 2013-01-01 00:00:00 UTC

	MSG_HELLO          = 0x00
	MSG_DATA_RECORDS   = 0x04
	MSG_COMMIT_REQUEST = 0x05
	MSG_VERSION        = 0x14
	MSG_ASYNC_SESSION  = 0x22
	MSG_SOCKET_CLOSE   = 0x26

	MSG_HELLO_RESPONSE         = 0x01
	MSG_COMMIT_RESPONSE        = 0x06
	MSG_ASYNC_SESSION_COMPLETE = 0x23

	FIELD_GPS       = 0x00
	FIELD_ANALOG_16 = 0x06
	FIELD_ANALOG_32 = 0x07
)

type Config struct {
	Port           string
	TraccarURL     string
	TraccarEnabled bool
}

type GPSData struct {
	Timestamp     uint32
	Latitude      float64
	Longitude     float64
	Altitude      int16
	GroundSpeed   uint16
	Heading       uint8
	PDOP          uint8
	PosAccuracy   uint8
	Valid         bool
}

type AnalogData struct {
	BatteryV float64
}

type DataRecord struct {
	Timestamp uint32
	GPS       *GPSData
	Analog    *AnalogData
}

func loadConfig() Config {
	port := getEnv("PORT", "20200")
	traccarURL := getEnv("TRACCAR_URL", "http://localhost:5055")
	traccarEnabled := getEnv("TRACCAR_ENABLED", "true") == "true"

	return Config{
		Port:           port,
		TraccarURL:     traccarURL,
		TraccarEnabled: traccarEnabled,
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func main() {
	config := loadConfig()

	listener, err := net.Listen("tcp", ":"+config.Port)
	if err != nil {
		log.Fatalf("Failed to listen on port %s: %v", config.Port, err)
	}
	defer listener.Close()

	log.Printf("Digital Matter Server listening on port %s", config.Port)
	if config.TraccarEnabled {
		log.Printf("Traccar forwarding enabled: %s", config.TraccarURL)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}

		go handleConnection(conn, config)
	}
}

func handleConnection(conn net.Conn, config Config) {
	defer conn.Close()

	buf := make([]byte, 4096)
	pendingData := []byte{}
	var deviceIMEI string

	for {
		conn.SetReadDeadline(time.Now().Add(10 * time.Minute))

		n, err := conn.Read(buf)
		if err != nil {
			if err != io.EOF && !isTimeout(err) {
				log.Printf("Read error from %s: %v", conn.RemoteAddr(), err)
			}
			break
		}

		if n == 0 {
			continue
		}

		data := append(pendingData, buf[:n]...)
		pendingData = []byte{}

		processedBytes, responses := processMessages(data, &deviceIMEI, config)

		if processedBytes < len(data) {
			pendingData = data[processedBytes:]
		}

		for _, response := range responses {
			if response != nil {
				if _, err := conn.Write(response); err != nil {
					log.Printf("Write error to %s: %v", conn.RemoteAddr(), err)
					return
				}
			}
		}
	}
}

func processMessages(data []byte, deviceIMEI *string, config Config) (int, [][]byte) {
	responses := [][]byte{}
	offset := 0

	for offset < len(data)-2 {
		if data[offset] != 0x02 || offset+1 >= len(data) || data[offset+1] != 0x55 {
			offset++
			continue
		}

		if offset+5 > len(data) {
			break
		}

		msgType := data[offset+2]
		payloadLen := binary.LittleEndian.Uint16(data[offset+3 : offset+5])
		totalLen := 5 + int(payloadLen)

		if offset+totalLen > len(data) {
			break
		}

		message := data[offset : offset+totalLen]

		imei := parseIMEI(message, msgType)
		if imei != "" {
			if *deviceIMEI == "" {
				*deviceIMEI = imei
				log.Printf("Connection from IMEI: %s", imei)
			}
		}

		if msgType == MSG_DATA_RECORDS {
			records := parseDataRecords(message)
			for _, record := range records {
				if record.GPS != nil && record.GPS.Valid {
					log.Printf("Got GPS data from IMEI %s: %.6f, %.6f", *deviceIMEI, record.GPS.Latitude, record.GPS.Longitude)

					if config.TraccarEnabled && *deviceIMEI != "" {
						battery := 0.0
						if record.Analog != nil {
							battery = record.Analog.BatteryV
						}

						if err := forwardToTraccar(config.TraccarURL, *deviceIMEI, record.GPS, record.Timestamp, battery); err != nil {
							log.Printf("Traccar forward error for IMEI %s: %v", *deviceIMEI, err)
						}
					}
				}
			}
		}

		response := buildResponse(msgType)
		if response != nil {
			responses = append(responses, response)
		}

		offset += totalLen
	}

	return offset, responses
}

func parseIMEI(data []byte, msgType uint8) string {
	if msgType != MSG_HELLO || len(data) < 9 {
		return ""
	}

	if len(data) > 9 {
		imeiEnd := 9
		for imeiEnd < len(data) && data[imeiEnd] != 0x00 {
			imeiEnd++
		}
		if imeiEnd > 9 {
			return string(data[9:imeiEnd])
		}
	}
	return ""
}

func parseDataRecords(data []byte) []DataRecord {
	records := []DataRecord{}

	if len(data) < 5 {
		return records
	}

	payload := data[5:]
	offset := 0

	for offset < len(payload) {
		if offset+11 > len(payload) {
			break
		}

		recordLen := binary.LittleEndian.Uint16(payload[offset : offset+2])
		if recordLen < 11 || offset+int(recordLen) > len(payload) {
			break
		}

		record := DataRecord{
			Timestamp: binary.LittleEndian.Uint32(payload[offset+6 : offset+10]),
		}

		fieldOffset := offset + 11
		for fieldOffset < offset+int(recordLen) {
			if fieldOffset+2 > len(payload) {
				break
			}

			fieldID := payload[fieldOffset]
			fieldLen := payload[fieldOffset+1]

			if fieldOffset+2+int(fieldLen) > len(payload) {
				break
			}

			fieldData := payload[fieldOffset+2 : fieldOffset+2+int(fieldLen)]

			switch fieldID {
			case FIELD_GPS:
				record.GPS = parseGPSField(fieldData)
			case FIELD_ANALOG_16:
				record.Analog = parseAnalog16Field(fieldData)
			case FIELD_ANALOG_32:
				record.Analog = parseAnalog32Field(fieldData)
			}

			fieldOffset += 2 + int(fieldLen)
		}

		records = append(records, record)
		offset += int(recordLen)
	}

	return records
}

func parseGPSField(data []byte) *GPSData {
	if len(data) < 21 {
		return nil
	}

	return &GPSData{
		Timestamp:   binary.LittleEndian.Uint32(data[0:4]),
		Latitude:    float64(int32(binary.LittleEndian.Uint32(data[4:8]))) / 10000000.0,
		Longitude:   float64(int32(binary.LittleEndian.Uint32(data[8:12]))) / 10000000.0,
		Altitude:    int16(binary.LittleEndian.Uint16(data[12:14])),
		GroundSpeed: binary.LittleEndian.Uint16(data[14:16]),
		Heading:     data[17],
		PDOP:        data[18],
		PosAccuracy: data[19],
		Valid:       true,
	}
}

func parseAnalog16Field(data []byte) *AnalogData {
	analog := &AnalogData{}

	for i := 0; i < len(data); {
		if i+2 >= len(data) {
			break
		}

		analogID := data[i]
		value := int16(binary.LittleEndian.Uint16(data[i+1 : i+3]))

		if analogID == 1 {
			analog.BatteryV = float64(value) / 1000.0
		}

		i += 3
	}

	return analog
}

func parseAnalog32Field(data []byte) *AnalogData {
	return &AnalogData{}
}

func forwardToTraccar(traccarURL, imei string, gps *GPSData, timestamp uint32, battery float64) error {
	params := url.Values{}
	params.Set("id", imei)
	params.Set("lat", fmt.Sprintf("%.6f", gps.Latitude))
	params.Set("lon", fmt.Sprintf("%.6f", gps.Longitude))

	unixTimestamp := int64(timestamp) + DM_EPOCH
	params.Set("timestamp", strconv.FormatInt(unixTimestamp, 10))

	if gps.Altitude != 0 {
		params.Set("altitude", strconv.Itoa(int(gps.Altitude)))
	}

	if gps.GroundSpeed > 0 {
		speedKnots := float64(gps.GroundSpeed) * 0.539957
		params.Set("speed", fmt.Sprintf("%.2f", speedKnots))
	}

	bearing := float64(gps.Heading) * 5.625
	if bearing > 360 {
		bearing -= 360
	}
	params.Set("bearing", fmt.Sprintf("%.1f", bearing))

	if gps.PosAccuracy > 0 {
		params.Set("accuracy", strconv.Itoa(int(gps.PosAccuracy)))
	}

	if gps.PDOP > 0 {
		params.Set("hdop", fmt.Sprintf("%.1f", float64(gps.PDOP)/10.0))
	}

	if battery > 0 {
		batteryPercent := ((battery - 3.0) / (4.5 - 3.0)) * 100.0
		if batteryPercent < 0 {
			batteryPercent = 0
		}
		if batteryPercent > 100 {
			batteryPercent = 100
		}
		params.Set("batt", fmt.Sprintf("%.1f", batteryPercent))
	}

	params.Set("valid", "true")

	fullURL := fmt.Sprintf("%s?%s", traccarURL, params.Encode())

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fullURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func buildResponse(msgType uint8) []byte {
	switch msgType {
	case MSG_HELLO:
		return buildHelloResponse()
	case MSG_COMMIT_REQUEST:
		return buildCommitResponse()
	case MSG_ASYNC_SESSION:
		return buildAsyncSessionCompleteResponse()
	default:
		return nil
	}
}

func buildHelloResponse() []byte {
	now := time.Now().Unix()
	dmTime := uint32(now - DM_EPOCH)

	response := make([]byte, 13)
	response[0] = 0x02
	response[1] = 0x55
	response[2] = MSG_HELLO_RESPONSE
	binary.LittleEndian.PutUint16(response[3:5], 0x0008)
	binary.LittleEndian.PutUint32(response[5:9], dmTime)

	return response
}

func buildCommitResponse() []byte {
	response := make([]byte, 6)
	response[0] = 0x02
	response[1] = 0x55
	response[2] = MSG_COMMIT_RESPONSE
	binary.LittleEndian.PutUint16(response[3:5], 0x0001)
	response[5] = 0x01

	return response
}

func buildAsyncSessionCompleteResponse() []byte {
	response := make([]byte, 5)
	response[0] = 0x02
	response[1] = 0x55
	response[2] = MSG_ASYNC_SESSION_COMPLETE
	binary.LittleEndian.PutUint16(response[3:5], 0x0000)

	return response
}

func isTimeout(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}