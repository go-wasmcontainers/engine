package api

import "os"

// DefaultSocket is where wctrd listens and wctr connects when neither -H
// nor WCTR_HOST says otherwise.
const DefaultSocket = "/var/run/wctr.sock"

// HostEnv names the environment variable that overrides the daemon host.
const HostEnv = "WCTR_HOST"

// DefaultHost is $WCTR_HOST if set, else unix:// + DefaultSocket.
func DefaultHost() string {
	if h := os.Getenv(HostEnv); h != "" {
		return h
	}
	return "unix://" + DefaultSocket
}
