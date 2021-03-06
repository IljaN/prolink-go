## Pioneer PRO DJ LINK client

This go library provides an API to the Pioneer PRO DJ LINK network. Providing
various forms of interactions.

Massive thank you to [@brunchboy](https://github.com/brunchboy) for his work on
[dysentery](https://github.com/brunchboy/dysentery).

[![GoDoc](https://godoc.org/go.evanpurkhiser.com/prolink?status.svg)](https://godoc.org/go.evanpurkhiser.com/prolink)

```go
import "go.evanpurkhiser.com/prolink"
```

### Basic usage

```go
config := prolink.Config {
    VirtualCDJID: 0x04,
}

network, err := prolink.Connect(config)

dm := network.DeviceManager()
st := network.CDJStatusMonitor()

added := func(dev *prolink.Device) {
    fmt.Printf("Connected: %s\n", dev)
}

removed := func(dev *prolink.Device) {
    fmt.Printf("Disconected: %s\n", dev)
}

dm.OnDeviceAdded(prolink.DeviceListenerFunc(added))
dm.OnDeviceRemoved(prolink.DeviceListenerFunc(removed))

statusChange := func(status *prolink.CDJStatus) {
    // Status packets come every 300ms, or faster depending on what is
    // happening on the CDJ. Do something with them.
}

st.OnStatusUpdate(prolink.StatusHandlerFunc(statusChange));
```

### Features

 * Listen for Pioneer PRO DJ LINK devices to connect and disconnect from the
   network using the
   [`DeviceManager`](https://godoc.org/go.evanpurkhiser.com/prolink#DeviceManager).
   Currently active devices may also be queried.

 * Receive Player status details for each CDJ on the network. The status is
   reported as
   [`CDJStatus`](https://godoc.org/go.evanpurkhiser.com/prolink#CDJStatus)
   structs.

 * Query the Rekordbox remoteDB server present on both CDJs themselves and on
   the Rekordbox (PC / OSX / Android / iOS) software for track metadata using
   [`RemoteDB`](https://godoc.org/go.evanpurkhiser.com/prolink#RemoteDB). This
   includes most metadata fields as well as (low quality) album artwork.

 * View the track status of an entire equipment setup as a whole using the
   [`trackstatus.Handler`](https://godoc.org/github.com/EvanPurkhiser/prolink-go/trackstatus#Handler).
   This allows you to determine the status of tracks in a mixing situation. Has
   the track been playing long enough to be considered 'now playing'?

### Limitations, bugs, and missing functionality

 * [[GH-1](https://github.com/EvanPurkhiser/prolink-go/issues/1)] Currently the
   software cannot be run on the same machine that is running Rekordbox.
   Rekordbox takes exclusive access to the socket used to communicate to the
   CDJs making it impossible to receive track status information

 * [[GH-3](https://github.com/EvanPurkhiser/prolink-go/issues/3)] When reading
   track metadata from USB devices some metadata is reported incorrectly.

 * [[GH-4](https://github.com/EvanPurkhiser/prolink-go/issues/4)] CD metadata
   cannot currently be read.

 * [Limitation?] To read track metadata from the CDJs USB drives you may have
   no more than 3 CDJs. Having 4 CDJs on the network will only allow you to
   read track metadata through linked Rekordbox.
