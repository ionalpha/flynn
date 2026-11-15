package diag

// Unknown is a counter the platform or application cannot report. It is negative so
// no reader can average, sum or threshold it into a false zero.
const Unknown = -1
