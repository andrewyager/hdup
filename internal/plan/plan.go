// Package plan calculates the parts of a multipart upload.
package plan

const MaxParts = 10000

type Part struct {
	Number int32
	Offset int64
	Size   int64
}

// PartSize doubles base until fileSize needs no more than MaxParts parts.
func PartSize(fileSize, base int64) int64 {
	size := base
	for fileSize > size*MaxParts {
		size *= 2
	}
	return size
}

func Parts(fileSize, base int64) []Part {
	size := PartSize(fileSize, base)
	var parts []Part
	for off := int64(0); off < fileSize; off += size {
		parts = append(parts, Part{
			Number: int32(len(parts) + 1),
			Offset: off,
			Size:   min(size, fileSize-off),
		})
	}
	return parts
}
