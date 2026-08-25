/// VYBE application entry point.
///
/// Deliberately thin. §0.6 names god files as a defect, and an entry point is
/// where they start: bootstrapping, routing, theming, and DI all accrete here
/// unless each has somewhere better to live.
///
/// What this file is allowed to do: install the ProviderScope, configure
/// localisation, and hand off. Nothing else.
library;

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import 'core/ui/tokens.dart';
import 'l10n/generated/app_localizations.dart';

void main() {
  WidgetsFlutterBinding.ensureInitialized();

  // ProviderScope is the DI root (ADR-001). Every dependency the app has is
  // reachable from here and overridable in a test via
  // ProviderContainer(overrides: [...]).
  runApp(const ProviderScope(child: VybeApp()));
}

class VybeApp extends ConsumerWidget {
  const VybeApp({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    return MaterialApp(
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

      home: const _BootstrapScreen(),
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

/// Placeholder root.
///
/// The five-tab shell of §3.1 lands in M1 with the vertical slice. This screen
/// exists so the app runs and the l10n pipeline is exercised end to end —
/// including at 200% scale and in RTL — before any screen depends on it.
///
/// It is honest about being a placeholder rather than a demo of nothing:
/// §0.3 rule 2 forbids real-looking UI over absent functionality.
class _BootstrapScreen extends StatelessWidget {
  const _BootstrapScreen();

  @override
  Widget build(BuildContext context) {
    final l10n = L10n.of(context);
    final theme = Theme.of(context);

    return Scaffold(
      body: SafeArea(
        child: Padding(
          padding: Space.screen,
          child: Column(
            mainAxisAlignment: MainAxisAlignment.center,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(
                l10n.appTitle,
                style: theme.textTheme.displaySmall?.copyWith(
                  fontSize: TypeScale.display,
                  fontWeight: FontWeight.w700,
                ),
              ),
              const SizedBox(height: Space.sm),
              Text(
                l10n.syncPrepareInstruction,
                style: theme.textTheme.bodyMedium?.copyWith(
                  fontSize: TypeScale.body,
                ),
              ),
              const SizedBox(height: Space.xl),
              Semantics(
                // §3.5: countdown state is announced, and never conveyed by
                // colour alone.
                liveRegion: true,
                child: Text(
                  l10n.syncNotMeasured,
                  style: theme.textTheme.labelLarge?.copyWith(
                    fontSize: TypeScale.label,
                    color: theme.colorScheme.onSurfaceVariant,
                  ),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
