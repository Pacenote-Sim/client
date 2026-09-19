package stamp

// region is the reserved region itself: the markers with nothing between
// them, laid out as data so that it sits in the executable file exactly like
// this, and the server can find it and write into it. It is read at run time
// through Read, which also keeps the linker from discarding it.
//
// It is spelled out byte by byte rather than built from the marker variables
// above, because a value built at init would exist in memory and not in the
// file, and it is the file the server stamps.
var region = [RegionSize]byte{
	// head marker: "PACENOTESTAMP_HD"
	'P', 'A', 'C', 'E', 'N', 'O', 'T', 'E', 'S', 'T', 'A', 'M', 'P', '_', 'H', 'D',
	// version 0, reserved, length 0, checksum 0, then PayloadMax zero bytes,
	// which the array's zero value supplies.
	offTail: 'P', 'A', 'C', 'E', 'N', 'O', 'T', 'E', 'S', 'T', 'A', 'M', 'P', '_', 'T', 'L',
}
