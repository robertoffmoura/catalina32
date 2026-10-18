package d3d9

import (
	"strconv"
	"strings"
)

// COM method names by Windows vtable slot (d3d9.h, dinput.h, dsound.h, d3dx9mesh.h), for log
// messages about methods the shims do not implement.
var methodNames = map[string][]string{
	"IDirect3D9": strings.Fields(`QueryInterface AddRef Release RegisterSoftwareDevice GetAdapterCount
		GetAdapterIdentifier GetAdapterModeCount EnumAdapterModes GetAdapterDisplayMode CheckDeviceType
		CheckDeviceFormat CheckDeviceMultiSampleType CheckDepthStencilMatch CheckDeviceFormatConversion
		GetDeviceCaps GetAdapterMonitor CreateDevice`),
	"IDirect3DDevice9": strings.Fields(`QueryInterface AddRef Release TestCooperativeLevel
		GetAvailableTextureMem EvictManagedResources GetDirect3D GetDeviceCaps GetDisplayMode
		GetCreationParameters SetCursorProperties SetCursorPosition ShowCursor CreateAdditionalSwapChain
		GetSwapChain GetNumberOfSwapChains Reset Present GetBackBuffer GetRasterStatus SetDialogBoxMode
		SetGammaRamp GetGammaRamp CreateTexture CreateVolumeTexture CreateCubeTexture CreateVertexBuffer
		CreateIndexBuffer CreateRenderTarget CreateDepthStencilSurface UpdateSurface UpdateTexture
		GetRenderTargetData GetFrontBufferData StretchRect ColorFill CreateOffscreenPlainSurface
		SetRenderTarget GetRenderTarget SetDepthStencilSurface GetDepthStencilSurface BeginScene EndScene
		Clear SetTransform GetTransform MultiplyTransform SetViewport GetViewport SetMaterial GetMaterial
		SetLight GetLight LightEnable GetLightEnable SetClipPlane GetClipPlane SetRenderState
		GetRenderState CreateStateBlock BeginStateBlock EndStateBlock SetClipStatus GetClipStatus
		GetTexture SetTexture GetTextureStageState SetTextureStageState GetSamplerState SetSamplerState
		ValidateDevice SetPaletteEntries GetPaletteEntries SetCurrentTexturePalette
		GetCurrentTexturePalette SetScissorRect GetScissorRect SetSoftwareVertexProcessing
		GetSoftwareVertexProcessing SetNPatchMode GetNPatchMode DrawPrimitive DrawIndexedPrimitive
		DrawPrimitiveUP DrawIndexedPrimitiveUP ProcessVertices CreateVertexDeclaration
		SetVertexDeclaration GetVertexDeclaration SetFVF GetFVF CreateVertexShader SetVertexShader
		GetVertexShader SetVertexShaderConstantF GetVertexShaderConstantF SetVertexShaderConstantI
		GetVertexShaderConstantI SetVertexShaderConstantB GetVertexShaderConstantB SetStreamSource
		GetStreamSource SetStreamSourceFreq GetStreamSourceFreq SetIndices GetIndices CreatePixelShader
		SetPixelShader GetPixelShader SetPixelShaderConstantF GetPixelShaderConstantF
		SetPixelShaderConstantI GetPixelShaderConstantI SetPixelShaderConstantB GetPixelShaderConstantB
		DrawRectPatch DrawTriPatch DeletePatch CreateQuery`),
	"IDirectInput8A": strings.Fields(`QueryInterface AddRef Release CreateDevice EnumDevices
		GetDeviceStatus RunControlPanel Initialize FindDevice EnumDevicesBySemantics ConfigureDevices`),
	"IDirectInputDevice8A": strings.Fields(`QueryInterface AddRef Release GetCapabilities EnumObjects
		GetProperty SetProperty Acquire Unacquire GetDeviceState GetDeviceData SetDataFormat
		SetEventNotification SetCooperativeLevel GetObjectInfo GetDeviceInfo RunControlPanel Initialize
		CreateEffect EnumEffects GetEffectInfo GetForceFeedbackState SendForceFeedbackCommand
		EnumCreatedEffectObjects Escape Poll SendDeviceData EnumEffectsInFile WriteEffectToFile
		BuildActionMap SetActionMap GetImageInfo`),
	"IDirectSound8": strings.Fields(`QueryInterface AddRef Release CreateSoundBuffer GetCaps
		DuplicateSoundBuffer SetCooperativeLevel Compact GetSpeakerConfig SetSpeakerConfig Initialize
		VerifyCertification`),
	"IDirectSoundBuffer8": strings.Fields(`QueryInterface AddRef Release GetCaps GetCurrentPosition
		GetFormat GetVolume GetPan GetFrequency GetStatus Initialize Lock Play SetCurrentPosition
		SetFormat SetVolume SetPan SetFrequency Stop Unlock Restore SetFX AcquireResources GetObjectInPath`),
	"ID3DXBuffer": strings.Fields(`QueryInterface AddRef Release GetBufferPointer GetBufferSize`),
}

// methodName returns iface::Name for a Windows vtable slot, or iface::slot N.
func methodName(iface string, slot int) string {
	if l := methodNames[iface]; slot >= 0 && slot < len(l) {
		return iface + "::" + l[slot]
	}
	return iface + "::slot " + strconv.Itoa(slot)
}
