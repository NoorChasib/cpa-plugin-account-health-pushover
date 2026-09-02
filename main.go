package main

// The embedded tzdata copy lets display-timezone resolve IANA zone names even
// inside minimal containers that ship without a system zoneinfo database.
import _ "time/tzdata"

func main() {}
