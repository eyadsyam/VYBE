/// VYBE application entry point.
///
/// Deliberately thin. §0.6 names god files as a defect, and an entry point is
/// where they start: bootstrapping, routing, theming, and DI all accrete here
/// unless each has somewhere better to live.
///
/// What this file is allowed to do: install the ProviderScope, configure
/// localisation, and hand off. Nothing else.
library;

import 'dart:io' show Platform;

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import 'app/providers.dart';
import 'app/router.dart';
import 'core/network/api_client.dart';
import 'core/ui/tokens.dart';
import 'l10n/generated/app_localizations.dart';

void main() {
  WidgetsFlutterBinding.ensureInitialized();

  // ProviderScope is the DI root (ADR-001). Every dependency the app has is
  // reachable from here and overridable in a test via
  // ProviderContainer(overrides: [...]).
  runApp(
    ProviderScope(
      overrides: [apiEndpointProvider.overrideWithValue(_endpoint())],
      child: const VybeApp(),
    ),
  );
}

/// Where the API lives for this build.
///
/// The Android emulator maps the host's loopback to 10.0.2.2, so `localhost`
/// there is the emulated device itself — a developer following the README on
/// Android would otherwise get a connection refused with nothing explaining
/// it. Everything else uses loopback directly.
///
/// A `--dart-define` wins over both, because that is how a real build points
/// at staging without editing source.
ApiEndpoint _endpoint() {
  const configured = String.fromEnvironment('VYBE_API_BASE_URL');
  if (configured.isNotEmpty) return ApiEndpoint(configured);

  if (!kIsWeb && Platform.isAndroid) return ApiEndpoint.androidEmulator;
  return ApiEndpoint.localhost;
}

/// The router, built once and kept alive for the app's lifetime.
///
/// A provider rather than a field, because it needs `ref` to watch the session
/// — and rebuilding a GoRouter on every widget rebuild would reset the
/// navigation stack under the user.
final routerProvider = Provider<GoRouter>((ref) => createRouter(ref));

class VybeApp extends ConsumerWidget {
  const VybeApp({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    return MaterialApp.router(
      routerConfig: ref.watch(routerProvider),
      onGenerateTitle: (context) => L10n.of(context).appTitle,

      // §3.6: Arabic and English both ship in V1. `supportedLocales` drives
      // RTL automatically — every layout uses directional properties, so
      // nothing needs a manual mirror.
      localizationsDelegates: L10n.localizationsDelegates,
      supportedLocales: L10n.supportedLocales,

      theme: _buildTheme(Brightness.light),
      darkTheme: _buildTheme(Brightness.dark),

      builder: (context, child) {
        // NFR-16 / §3.5: text scales to 200%. We clamp the *upper* bound only
        // — a user who has chosen 250% at OS level gets 200% here, which is
        // the largest size every V1 screen is golden-tested against. Clamping
        // the lower bound would override an accessibility preference in the
        // other direction, so we do not.
        final media = MediaQuery.of(context);
        return MediaQuery(
          data: media.copyWith(
            textScaler: media.textScaler.clamp(
              maxScaleFactor: TypeScale.maxSupportedScale,
            ),
          ),
          child: child ?? const SizedBox.shrink(),
        );
      },
    );
  }
}

ThemeData _buildTheme(Brightness brightness) {
  // Seeded scheme for now. The real palette lands with the design system in
  // M1; seeding from one colour keeps contrast ratios coherent in the interim
  // rather than hand-picking values that would fail §3.5.
  final scheme = ColorScheme.fromSeed(
    seedColor: const Color(0xFF6C4DF6),
    brightness: brightness,
  );

  return ThemeData(
    colorScheme: scheme,
    useMaterial3: true,

    // §3.5: minimum 48x48dp touch target on every interactive element.
    // Setting it in the theme means a button has to opt *out* to be wrong.
    materialTapTargetSize: MaterialTapTargetSize.padded,

    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(
        minimumSize: const Size(Space.minTouchTarget, Space.minTouchTarget),
      ),
    ),
    iconButtonTheme: IconButtonThemeData(
      style: IconButton.styleFrom(
        minimumSize: const Size(Space.minTouchTarget, Space.minTouchTarget),
      ),
    ),
    cardTheme: CardThemeData(
      shape: const RoundedRectangleBorder(borderRadius: Radii.cardBorder),
      margin: EdgeInsets.zero,
    ),
  );
}
