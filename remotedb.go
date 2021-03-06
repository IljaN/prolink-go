package prolink

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"sync"
	"time"
	"unicode/utf16"
)

// ErrDeviceNotLinked is returned by RemoteDB if the device being queried is
// not currently 'linked' on the network.
var ErrDeviceNotLinked = fmt.Errorf("The device is not linked on the network")

// ErrCDUnsupported is returned when attempting to read metadata from a CD slot.
// TODO: Figure out what packet sequence is needed to read CD metadata.
var ErrCDUnsupported = fmt.Errorf("Reading metadata from CDs is currently unsupported")

// rdSeparator is a 6 byte marker used in TCP packets sent sent and received
// from the remote db server. It's not particular known exactly what this
// value is for, but in some packets it seems to be used as a field separator.
var rdSeparator = []byte{0x11, 0x87, 0x23, 0x49, 0xae, 0x11}

// buildPacket constructs a packet to be sent to remote database.
func buildPacket(messageID uint32, part []byte) []byte {
	count := make([]byte, 4)
	binary.BigEndian.PutUint32(count, messageID)

	header := bytes.Join([][]byte{rdSeparator, count}, nil)

	return append(header, part...)
}

// Given a byte array where the first 4 bytes contain the uint32 length of the
// string (number of runes) followed by a UTF-16 representation of the string,
// convert it to a string.
func stringFromUTF16(s []byte) string {
	size := binary.BigEndian.Uint32(s[:4])
	s = s[4:][:size*2]

	str16Bit := make([]uint16, 0, size)
	for ; len(s) > 0; s = s[2:] {
		str16Bit = append(str16Bit, binary.BigEndian.Uint16(s[:2]))
	}

	return string(utf16.Decode(str16Bit))[:size-1]
}

// rbDBServerQueryPort is the consistent port on which we can query the remote
// db server for the port to connect to to communicate with it.
const rbDBServerQueryPort = 12523

// getRemoteDBServerAddr queries the remote device for the port that the remote
// database server is listening on for requests.
func getRemoteDBServerAddr(deviceIP net.IP) (string, error) {
	addr := fmt.Sprintf("%s:%d", deviceIP, rbDBServerQueryPort)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return "", err
	}

	defer conn.Close()

	parts := [][]byte{
		[]byte{0x00, 0x00, 0x00, 0x0f},
		[]byte("RemoteDBServer"),
		[]byte{0x00},
	}

	queryPacket := bytes.Join(parts, nil)

	// Request for the port
	_, err = conn.Write(queryPacket)
	if err != nil {
		return "", fmt.Errorf("Failed to query remote DB Server port: %s", err)
	}

	// Read request response, should be a two byte uint16
	data := make([]byte, 2)

	_, err = conn.Read(data)
	if err != nil {
		return "", fmt.Errorf("Failed to retrieve remote DB Server port: %s", err)
	}

	port := binary.BigEndian.Uint16(data)

	return fmt.Sprintf("%s:%d", deviceIP, port), nil
}

type deviceConnection struct {
	remoteDB *RemoteDB
	device   *Device
	lock     *sync.Mutex
	conn     net.Conn
	msgCount uint32

	retryEvery time.Duration
	disconnect chan bool
}

// connect attempts to open a TCP socket connection  to the device. This will
// send the necessary packet sequence in order start communicating with the
// database server once connected.
func (dc *deviceConnection) connect() error {
	addr, err := getRemoteDBServerAddr(dc.device.IP)
	if err != nil {
		return err
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}

	// Begin connection to the remote database
	if _, err = conn.Write([]byte{0x11, 0x00, 0x00, 0x00, 0x01}); err != nil {
		return fmt.Errorf("Failed to connect to remote database: %s", err)
	}

	// No need to keep this response, but it *should* be 5 bytes
	io.CopyN(ioutil.Discard, conn, 5)

	// Send identification to the remote database
	identifyParts := [][]byte{
		rdSeparator,

		// Possible mask to reset the message counter (?)
		[]byte{0xff, 0xff, 0xff, 0xfe},

		// Currently don't know what these bytes do, but they're needed to get
		// the connection into a state where we can make queries
		[]byte{
			0x10, 0x00, 0x00, 0x0f, 0x01, 0x14, 0x00, 0x00,
			0x00, 0x0c, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x11, 0x00,
			0x00, 0x00,
		},

		// The last byte of the identifier is the device ID that we are assuming
		// to use to communicate with the remote database
		[]byte{byte(dc.remoteDB.deviceID)},
	}

	if _, err = conn.Write(bytes.Join(identifyParts, nil)); err != nil {
		return fmt.Errorf("Failed to connect to remote database: %s", err)
	}

	// No need to keep this response, but it *should be 42 bytes
	io.CopyN(ioutil.Discard, conn, 42)

	dc.conn = conn

	return nil
}

func (dc *deviceConnection) tryConnect(ticker *time.Ticker) bool {
	select {
	case <-dc.disconnect:
		return true
	case <-ticker.C:
		return dc.connect() == nil
	}
}

func (dc *deviceConnection) ensureConnect() {
	dc.disconnect = make(chan bool, 1)
	ticker := time.NewTicker(dc.retryEvery)

	// Attempt to immediately connect
	dc.connect()

	for dc.conn == nil && !dc.tryConnect(ticker) {
	}

	ticker.Stop()
}

// Open begins attempting to connect to the device. If we're unable to connect
// to the device we will retry until the deviceConnection is closed.
func (dc *deviceConnection) Open() {
	go dc.ensureConnect()
}

// Close stops any attempts to connect to the device or closes any open socket
// connections with the device.
func (dc *deviceConnection) Close() {
	if dc.disconnect != nil {
		dc.disconnect <- true
		close(dc.disconnect)
	}

	if dc.conn != nil {
		dc.conn.Close()
		dc.conn = nil
	}
}

// Track contains track information retrieved from the remote database.
type Track struct {
	ID      uint32
	Path    string
	Title   string
	Artist  string
	Album   string
	Label   string
	Genre   string
	Comment string
	Key     string
	Length  time.Duration
	Artwork []byte
}

// TrackQuery is used to make queries for track metadata.
type TrackQuery struct {
	TrackID  uint32
	Slot     TrackSlot
	DeviceID DeviceID

	// artworkID will be filled in after the track metadata is queried, this
	// feild will be needed to lookup the track artwork.
	artworkID uint32
}

// RemoteDB provides an interface to talking to the remote database.
type RemoteDB struct {
	deviceID DeviceID
	conns    map[DeviceID]*deviceConnection
}

// IsLinked reports weather the DB server is available for the given device.
func (rd *RemoteDB) IsLinked(devID DeviceID) bool {
	devConn, ok := rd.conns[devID]

	return ok && devConn.conn != nil
}

// GetTrack queries the remote db for track details given a track ID.
func (rd *RemoteDB) GetTrack(q *TrackQuery) (*Track, error) {
	if !rd.IsLinked(q.DeviceID) {
		return nil, ErrDeviceNotLinked
	}

	if q.Slot == TrackSlotCD {
		return nil, ErrCDUnsupported
	}

	track, err := rd.executeQuery(q)

	// Refresh the connection if we EOF while querying the server
	if err != nil && err == io.EOF {
		rd.refreshConnection(rd.conns[q.DeviceID].device)
	}

	return track, err
}

func (rd *RemoteDB) executeQuery(q *TrackQuery) (*Track, error) {
	// Synchroize queries as not to distruct the query flow. We could probably
	// be a little more precice about where the locks are, but for now the
	// entire query is pretty fast, just lock the whole thing.
	rd.conns[q.DeviceID].lock.Lock()
	defer rd.conns[q.DeviceID].lock.Unlock()

	track, err := rd.queryTrackMetadata(q)
	if err != nil {
		return nil, err
	}

	path, err := rd.queryTrackPath(q)
	if err != nil {
		return nil, err
	}

	track.Path = path

	// No artwork, nothing left to do
	if binary.BigEndian.Uint32(track.Artwork) == 0 {
		// Empty the 4byte artwork ID so that it matches the empty value.
		track.Artwork = nil

		return track, nil
	}

	q.artworkID = binary.BigEndian.Uint32(track.Artwork)

	artwork, err := rd.queryArtwork(q)
	if err != nil {
		return nil, err
	}

	track.Artwork = artwork

	return track, nil
}

// queryTrackMetadata queries the rmote database for various metadata about a
// track, returing a sparse Track object. The track Path and Artwork must be
// looked up as separate queries.
//
// Note that the Artwork ID is populated in the Artwork field, as this value is
// returned with the track metadata and is needed to lookup the artwork.
func (rd *RemoteDB) queryTrackMetadata(q *TrackQuery) (*Track, error) {
	trackID := make([]byte, 4)
	binary.BigEndian.PutUint32(trackID, q.TrackID)

	dvID := byte(rd.deviceID)
	slot := byte(q.Slot)

	part1 := []byte{
		0x10, 0x20, 0x02, 0x0f, 0x02, 0x14, 0x00, 0x00,
		0x00, 0x0c, 0x06, 0x06, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x11, dvID,
		0x01, slot, 0x01, 0x11,
	}
	part1 = append(part1, trackID...)

	part2 := []byte{
		0x10, 0x30, 0x00, 0x0f, 0x06, 0x14, 0x00, 0x00,
		0x00, 0x0c, 0x06, 0x06, 0x06, 0x06, 0x06, 0x06,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x11, dvID,
		0x01, slot, 0x01, 0x11, 0x00, 0x00, 0x00, 0x00,
		0x11, 0x00, 0x00, 0x00, 0x0b, 0x11, 0x00, 0x00,
		0x00, 0x00, 0x11, 0x00, 0x00, 0x00, 0x0b, 0x11,
		0x00, 0x00, 0x00, 0x00,
	}

	items, err := rd.getMultimessageResp(q.DeviceID, part1, part2)
	if err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(items[3][28:32])

	track := &Track{
		ID:      q.TrackID,
		Title:   stringFromUTF16(items[0][38:]),
		Artist:  stringFromUTF16(items[1][38:]),
		Album:   stringFromUTF16(items[2][38:]),
		Comment: stringFromUTF16(items[5][38:]),
		Key:     stringFromUTF16(items[6][38:]),
		Genre:   stringFromUTF16(items[9][38:]),
		Label:   stringFromUTF16(items[10][38:]),
		Length:  time.Duration(length) * time.Second,

		// Artwork will be given in with the artwork ID
		Artwork: items[0][len(items[0])-19:][:4],
	}

	return track, nil
}

// queryTrackPath looks up the file path of a track in rekordbox.
func (rd *RemoteDB) queryTrackPath(q *TrackQuery) (string, error) {
	trackID := make([]byte, 4)
	binary.BigEndian.PutUint32(trackID, q.TrackID)

	dvID := byte(rd.deviceID)
	slot := byte(q.Slot)

	part1 := []byte{
		0x10, 0x21, 0x02, 0x0f, 0x02, 0x14, 0x00, 0x00,
		0x00, 0x0c, 0x06, 0x06, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x11, dvID,
		0x08, slot, 0x01, 0x11,
	}
	part1 = append(part1, trackID...)

	part2 := []byte{
		0x10, 0x30, 0x00, 0x0f, 0x06, 0x14, 0x00, 0x00,
		0x00, 0x0c, 0x06, 0x06, 0x06, 0x06, 0x06, 0x06,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x11, dvID,
		0x08, slot, 0x01, 0x11, 0x00, 0x00, 0x00, 0x00,
		0x11, 0x00, 0x00, 0x00, 0x06, 0x11, 0x00, 0x00,
		0x00, 0x00, 0x11, 0x00, 0x00, 0x00, 0x06, 0x11,
		0x00, 0x00, 0x00, 0x00,
	}

	items, err := rd.getMultimessageResp(q.DeviceID, part1, part2)
	if err != nil {
		return "", err
	}

	return stringFromUTF16(items[4][38:]), nil
}

// getMultimessageResp is used for queries that that multiple packets to setup
// and respond with mult-section bodies that can be split on the rbSection
// delimiter.
func (rd *RemoteDB) getMultimessageResp(devID DeviceID, p1, p2 []byte) ([][]byte, error) {
	// Part one of query
	packet := buildPacket(rd.conns[devID].msgCount, p1)

	if err := rd.sendMessage(devID, packet); err != nil {
		return nil, err
	}

	messageID := rd.conns[devID].msgCount

	// This data doesn't seem useful, there *should* be 42 bytes of it
	io.CopyN(ioutil.Discard, rd.conns[devID].conn, 42)

	// Part two of query
	packet = buildPacket(messageID, p2)

	// As far as I can tell, these multi-section packets *do not* have a length
	// marker for bytes in the message, or even how many sections they will
	// have. So for now, look for the 'final section' which seems to always be
	// empty. We can reuse buildPacket here even though this is not a packet.
	finalSection := buildPacket(messageID, []byte{
		0x10, 0x42, 0x01, 0x0f, 0x00, 0x14, 0x00, 0x00, 0x00,
		0x0c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	})

	if err := rd.sendMessage(devID, packet); err != nil {
		return nil, err
	}

	part := make([]byte, 1024)
	full := []byte{}

	for !bytes.HasSuffix(full, finalSection) {
		n, err := rd.conns[devID].conn.Read(part)
		if err != nil {
			return nil, err
		}

		full = append(full, part[:n]...)
	}

	// Break into sections (keep only interesting ones
	sections := bytes.Split(full, rdSeparator)[2:]
	sections = sections[:len(sections)-1]

	// Remove uint32 message counter from each section
	for i := range sections {
		sections[i] = sections[i][4:]
	}

	return sections, nil
}

// queryArtwork requests artwork of a specific ID from the remote database.
func (rd *RemoteDB) queryArtwork(q *TrackQuery) ([]byte, error) {
	artID := make([]byte, 4)
	binary.BigEndian.PutUint32(artID, q.artworkID)

	dvID := byte(rd.deviceID)
	slot := byte(q.Slot)

	part := []byte{
		0x10, 0x20, 0x03, 0x0f, 0x02, 0x14, 0x00, 0x00,
		0x00, 0x0c, 0x06, 0x06, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x11, dvID,
		0x08, slot, 0x01, 0x11,
	}
	part = append(part, artID...)

	packet := buildPacket(rd.conns[q.DeviceID].msgCount, part)

	if err := rd.sendMessage(q.DeviceID, packet); err != nil {
		return nil, err
	}

	// there is a uint32 at byte 48 containing the size of the image, simply
	// read up until this value so we know how much more to read after.
	data := make([]byte, 52)

	_, err := rd.conns[q.DeviceID].conn.Read(data)
	if err != nil {
		return nil, err
	}

	imgLen := binary.BigEndian.Uint32(data[48:52])
	img := make([]byte, int(imgLen))

	_, err = io.ReadFull(rd.conns[q.DeviceID].conn, img)
	if err != nil {
		return nil, err
	}

	return img, nil
}

// sendMessage writes to the open connection and increments the message
// counter.
func (rd *RemoteDB) sendMessage(devID DeviceID, m []byte) error {
	devConn := rd.conns[devID]

	if _, err := devConn.conn.Write(m); err != nil {
		return err
	}

	devConn.msgCount++

	return nil
}

// openConnection initializes a new deviceConnection for the specified device.
func (rd *RemoteDB) openConnection(dev *Device) {
	conn := &deviceConnection{
		remoteDB:   rd,
		device:     dev,
		lock:       &sync.Mutex{},
		msgCount:   1,
		retryEvery: 5 * time.Second,
	}

	conn.Open()
	rd.conns[dev.ID] = conn
}

// refreshConnection attempts to reconnect to the specified device.
func (rd *RemoteDB) refreshConnection(dev *Device) {
	rd.closeConnection(dev)
	rd.openConnection(dev)
}

// closeConnection closes the active connection for the specified device.
func (rd *RemoteDB) closeConnection(dev *Device) {
	rd.conns[dev.ID].Close()
	delete(rd.conns, dev.ID)
}

// activate begins actively listening for devices on the network hat support
// remote database queries to be added to the PRO DJ LINK network. This
// maintains adding and removing of device connections.
func (rd *RemoteDB) activate(dm *DeviceManager, deviceID DeviceID) {
	rd.deviceID = deviceID

	allowedDevices := map[DeviceType]bool{
		DeviceTypeRB:  true,
		DeviceTypeCDJ: true,
	}

	// Cleanup devices removed from the network
	onRemove := rd.closeConnection

	// Connect to the remote database of new devices on the network
	onConnect := func(dev *Device) {
		// Not all pro-link devices provide the remote DB service
		if _, ok := allowedDevices[dev.Type]; ok {
			rd.openConnection(dev)
		}
	}

	dm.OnDeviceAdded(DeviceListenerFunc(onConnect))
	dm.OnDeviceRemoved(DeviceListenerFunc(onRemove))
}

func newRemoteDB() *RemoteDB {
	return &RemoteDB{
		conns: map[DeviceID]*deviceConnection{},
	}
}
