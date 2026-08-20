package server

import "syscall"

// syscallEADDRINUSE is split out so the port-walking check can use errors.Is
// against the platform's own error value rather than only matching a string.
var syscallEADDRINUSE = syscall.EADDRINUSE
