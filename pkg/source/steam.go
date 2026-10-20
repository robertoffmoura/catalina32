package source

import "catalina32/pkg/cpu"

// Steam, for now absent: SteamAPI_Init fails (the game then runs without Steam features, as on
// a Mac where the client isn't running). The flat API's accessors still return a context, as
// libsteam_api does, whose interface pointers are NULL.

func (m *Machine) registerSteamShims() {
	for k, v := range map[string]cpu.ThunkFunc{
		"_SteamAPI_Init":                           ret0,
		"_SteamAPI_InitSafe":                       func(c *cpu.CPU) { m.logf("[steam] SteamAPI_InitSafe: no Steam client"); ret(c, 0) },
		"_SteamInternal_SteamAPI_Init":             ret0,
		"_SteamAPI_IsSteamRunning":                 ret0,
		"_SteamAPI_RestartAppIfNecessary":          ret0,
		"_SteamAPI_Shutdown":                       retVoid,
		"_SteamAPI_RunCallbacks":                   retVoid,
		"_SteamAPI_RegisterCallback":               retVoid,
		"_SteamAPI_UnregisterCallback":             retVoid,
		"_SteamAPI_RegisterCallResult":             retVoid,
		"_SteamAPI_UnregisterCallResult":           retVoid,
		"_SteamAPI_UseBreakpadCrashHandler":        retVoid,
		"_SteamAPI_SetBreakpadAppID":               retVoid,
		"_SteamAPI_SetTryCatchCallbacks":           retVoid,
		"_SteamAPI_GetHSteamUser":                  ret0,
		"_SteamAPI_GetHSteamPipe":                  ret0,
		"_SteamClient":                             ret0,
		"_SteamInternal_CreateInterface":           ret0,
		"_SteamInternal_FindOrCreateUserInterface": ret0,
		// struct { void (*init)(void *ctx); uintptr_t counter; context } -> &context, left zeroed.
		"_SteamInternal_ContextInit": func(c *cpu.CPU) { ret(c, c.Arg(0)+8) },
	} {
		m.shims[k] = v
	}
}
