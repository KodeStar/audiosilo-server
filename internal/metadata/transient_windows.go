//go:build windows

package metadata

import "syscall"

// platformTransientErrnos are TransientFault's Windows codes. A Windows file
// call fails with Windows' own error codes, never syscall.EIO and the other
// names on the list (values no Windows call returns), so without these a
// mapped or UNC share whose session dropped would read as a lasting fault (a
// 404, a podcast episode fetched again) where it is one a retry may get past.
var platformTransientErrnos = []syscall.Errno{
	53,   // ERROR_BAD_NETPATH: the network path was not found (its server down)
	54,   // ERROR_NETWORK_BUSY
	55,   // ERROR_DEV_NOT_EXIST: the network resource is no longer available
	59,   // ERROR_UNEXP_NET_ERR
	64,   // ERROR_NETNAME_DELETED: the network name is no longer available
	121,  // ERROR_SEM_TIMEOUT: the share did not answer in time
	1117, // ERROR_IO_DEVICE: an I/O device error (EIO's)
	1225, // ERROR_CONNECTION_REFUSED
	1231, // ERROR_NETWORK_UNREACHABLE
	1232, // ERROR_HOST_UNREACHABLE
	1236, // ERROR_CONNECTION_ABORTED
}

// platformLastingErrnos are LastingFault's Windows codes: a name the share
// refuses (EINVAL's on a POSIX share).
var platformLastingErrnos = []syscall.Errno{
	123, // ERROR_INVALID_NAME
}
