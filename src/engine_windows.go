//go:build windows

package main

import _ "embed"

//go:embed assets/win-vulkan.zip
var winVulkanZip []byte

//go:embed assets/win-cpu.zip
var winCPUZip []byte

type engineBlob struct {
	Kind string // subfolder name under engine/
	Data []byte
	Tar  bool
}

func engineBlobs() []engineBlob {
	return []engineBlob{
		{Kind: "vulkan", Data: winVulkanZip},
		{Kind: "cpu", Data: winCPUZip},
	}
}

// Engine launch candidates, best first: GPU (Vulkan) then CPU fallback.
func engineCandidates() []engineCand {
	return []engineCand{
		{Dir: "vulkan", ExeName: "llama-server.exe", NGL: 999, Name: "GPU (Vulkan)"},
		{Dir: "cpu", ExeName: "llama-server.exe", NGL: 0, Name: "CPU"},
	}
}
