#include <stdlib.h>
__declspec(dllimport) void uwp_GetBundlePath(char* buffer);
__declspec(dllimport) void uwp_GetBundleFilePath(char* buffer, const char *filename);
__declspec(dllimport) void uwp_GetScreenSize(int* x, int* y);
__declspec(dllimport) void uwp_GetTarget(char* buffer);
__declspec(dllimport) void uwp_LogMessage(char* buffer);
__declspec(dllimport) void uwp_PatchFolder(char* folder);
__declspec(dllimport) void uwp_PickAFolder(char* buffer);
