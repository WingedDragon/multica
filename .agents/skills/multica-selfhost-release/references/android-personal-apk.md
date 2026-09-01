# Personal selfhost Android APK

Use this reference when the user wants an installable Android release APK for a
personal device, not Google Play publication. It builds only the client; do not
run the server deployment workflow.

## Invariants

- Read the deployed host's public `MULTICA_APP_URL` from
  `dj:~/apps/multica/.env`. Bake that HTTPS origin into both
  `EXPO_PUBLIC_API_URL` and `EXPO_PUBLIC_WEB_URL`; never use the committed
  public-production endpoint.
- Create the ignored `apps/mobile/.env.production.local` for this selfhost.
  Expo gives it higher priority than the committed `.env.production`, including
  for Gradle's Expo CLI subprocess.
- Keep the Android application ID stable (`club.zxyh.multica` for the current
  selfhost) and raise `EXPO_ANDROID_VERSION_CODE` for every update installed
  over an existing app.
- Android rejects an unsigned APK. Expo's generated project signs its `release`
  variant with its local debug keystore. This is suitable for a private device,
  requires neither Google Play nor EAS credentials, and is not a public-release
  signature.
- `android/` is generated and ignored. Regenerate it for every build; do not
  commit it or hand-edit it outside this one build workflow.

## Required generated-project compatibility overrides

React Native 0.83.6 applies `foojay-resolver-convention` 0.5.0 in its included
Gradle build. That resolver references `JvmVendorSpec.IBM_SEMERU`, removed by
Gradle 9. Expo prebuild currently generates a Gradle 9 wrapper, so change the
generated wrapper to Gradle 8.14.3 before assembling.

The React Native Gradle plugin sends Expo's `export:embed` command
`--minify false` when Hermes is enabled. In this project that leaves
`EXPO_PUBLIC_*` values resolved to the committed public production file even
when the selfhost local override was loaded. Add the generated
`extraPackagerArgs = ["--minify", "true"]` line to force the production
inlining path. Verify the final Hermes bytecode; a successful Gradle build alone
does not prove the app targets the selfhost.

## Build

Run from the repository root. Obtain only the public origin from the selfhost
configuration:

```bash
selfhost_url="$(ssh dj "sed -n 's/^MULTICA_APP_URL=//p' ~/apps/multica/.env")"
test -n "$selfhost_url"
case "$selfhost_url" in https://*) ;; *) echo "Expected HTTPS selfhost URL" >&2; exit 1 ;; esac
```

Write the ignored local override, then generate the Android project:

```bash
cat > apps/mobile/.env.production.local <<EOF
EXPO_PUBLIC_API_URL=$selfhost_url
EXPO_PUBLIC_WEB_URL=$selfhost_url
EXPO_ANDROID_PACKAGE=club.zxyh.multica
EXPO_ANDROID_VERSION_CODE=1
EOF

NODE_ENV=production APP_ENV=production \
pnpm --filter @multica/mobile exec expo prebuild --platform android --clean --no-install
```

Make the two required generated-file overrides:

1. In `apps/mobile/android/gradle/wrapper/gradle-wrapper.properties`, replace
   `gradle-9.0.0-bin` with `gradle-8.14.3-bin`.
2. In the `react {}` block of `apps/mobile/android/app/build.gradle`, insert
   this line immediately after the `extraPackagerArgs = []` example comment:

   ```groovy
   extraPackagerArgs = ["--minify", "true"]
   ```

Build without reusing a Gradle daemon, so the production environment is loaded
for this exact artifact:

```bash
ANDROID_HOME=/opt/homebrew/share/android-commandlinetools
export ANDROID_HOME ANDROID_SDK_ROOT="$ANDROID_HOME"

(
  cd apps/mobile/android
  NODE_ENV=production APP_ENV=production ./gradlew --no-daemon :app:assembleRelease
)

mkdir -p apps/mobile/dist
cp apps/mobile/android/app/build/outputs/apk/release/app-release.apk \
  apps/mobile/dist/Multica-selfhost-release.apk
```

## Verify before handing off

1. Inspect `assets/app.config` inside the APK. It must identify `Multica`,
   `APP_ENV=production`, and Android package `club.zxyh.multica`.
2. Use the installed Android build tools to prove package ID and signature:

   ```bash
   apk=apps/mobile/dist/Multica-selfhost-release.apk
   build_tools="$ANDROID_HOME/build-tools/36.0.0"

   "$build_tools/aapt" dump badging "$apk"
   "$build_tools/apksigner" verify --verbose --print-certs "$apk"
   ```

3. `assets/index.android.bundle` is Hermes bytecode; `strings` cannot prove its
   endpoint. Disassemble it with the installed `hermesc` binary and assert that
   `https://multica.zxyh.club` is present and `https://api.multica.ai` is
   absent.

## Wireless ADB installation

For Android 11 or later, enable **Developer options → Wireless debugging** on
the phone, then select its pairing-code flow. Pairing and debugging use
different ports: pair with the pairing port, then connect with the debugging
port.

```bash
adb pair <phone-ip>:<pairing-port>
# Enter the six-digit pairing code displayed on the phone.

adb connect <phone-ip>:<debug-port>
adb devices -l
```

The TCP serial from `adb devices -l` has the form
`<phone-ip>:<debug-port>`. Prefer it over an automatic mDNS transport. A single
physical phone can appear twice—once by TCP and once by mDNS—which makes a bare
`adb install` fail with `more than one device/emulator`.

Disconnect the redundant mDNS serial shown by that command, then set
`ANDROID_SERIAL` once for the current terminal:

```bash
adb disconnect <mDNS-serial>
export ANDROID_SERIAL=<phone-ip>:<debug-port>

adb install -r apps/mobile/dist/Multica-selfhost-release.apk
```

With `ANDROID_SERIAL` set, bare `adb install`, `adb shell`, and `adb logcat`
all target that phone. The debugging port can rotate after disabling wireless
debugging or reconnecting it; run `adb connect` and update `ANDROID_SERIAL`
when that happens.

Keep the phone unlocked and accept its installation confirmation. An
`INSTALL_FAILED_USER_RESTRICTED: Install canceled by user` result is a
phone-side cancellation or policy restriction, not an APK-signing failure. If
it persists after explicit confirmation, enable the vendor-specific
**Install via USB** or **USB debugging (Security settings)** option in
Developer options, then retry the same targeted command.

Authenticate against the selfhost after the app launches. Do not claim device
verification if no connected device completed this step.
