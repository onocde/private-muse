//go:build linux

package main

import _ "embed"

//go:embed assets/ubuntu-x64.tar.gz
var ubuntuTar []byte

type engineBlob struct {
	Kind string // subfolder name under engine/
	Data []byte
	Tar  bool
}

func engineBlobs() []engineBlob {
	return []engineBlob{
		{Kind: "main", Data: ubuntuTar, Tar: true},
	}
}

func engineCandidates() []engineCand {
	return []engineCand{
		{Dir: "main", ExeName: "llama-server", NGL: 0, Name: "CPU"},
	}
}
