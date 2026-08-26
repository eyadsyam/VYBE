/// Shared setup for golden tests (§13.2).
///
/// Every V1 screen is captured in **eight** combinations: English and Arabic,
/// light and dark, at 100% and 200% text scale. That matrix is not
/// thoroughness for its own sake — each axis catches a different class of bug
/// that nothing else in the suite can:
///
///   * **Arabic** catches a layout built with `left`/`right` instead of
///     `start`/`end`, which looks perfect in English and is mirrored wrong in
///     Arabic.
///   * **Dark** catches a hardcoded colour, which is invisible until somebody
///     with dark mode on opens the screen.
///   * **200%** catches an overflow (NFR-16). A `Row` that fits at 100% and
///     overflows at 200% is the single most common accessibility failure in
///     Flutter, and it never shows up in a widget test that only asserts a
///     string is present.
library;

import 'package:flutter/material.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_riverpod/misc.dart' show Override;
import 'package:flutter_test/flutter_test.dart';
import 'package:vybe/core/ui/tokens.dart';
import 'package:vybe/l10n/generated/app_localizations.dart';

/// One cell of the golden matrix.
class GoldenVariant {
  const GoldenVariant({
    required this.name,
    required this.locale,
    required this.brightness,
    required this.textScale,
  });

  final String name;
  final Locale locale;
  final Brightness brightness;
  final double textScale;

  @override
  String toString() => name;
}

/// The full matrix: 2 locales x 2 themes x 2 scales.
const goldenVariants = <GoldenVariant>[
  GoldenVariant(
    name: 'en_light_1x',
    locale: Locale('en'),
    brightness: Brightness.light,
    textScale: 1,
  ),
  GoldenVariant(
    name: 'en_light_2x',
    locale: Locale('en'),
    brightness: Brightness.light,
    textScale: 2,
  ),
  GoldenVariant(
    name: 'en_dark_1x',
    locale: Locale('en'),
    brightness: Brightness.dark,
    textScale: 1,
  ),
  GoldenVariant(
    name: 'en_dark_2x',
    locale: Locale('en'),
    brightness: Brightness.dark,
    textScale: 2,
  ),
  GoldenVariant(
    name: 'ar_light_1x',
    locale: Locale('ar'),
    brightness: Brightness.light,
    textScale: 1,
  ),
  GoldenVariant(
    name: 'ar_light_2x',
    locale: Locale('ar'),
    brightness: Brightness.light,
    textScale: 2,
  ),
  GoldenVariant(
    name: 'ar_dark_1x',
    locale: Locale('ar'),
    brightness: Brightness.dark,
    textScale: 1,
  ),
  GoldenVariant(
    name: 'ar_dark_2x',
    locale: Locale('ar'),
    brightness: Brightness.dark,
    textScale: 2,
  ),
];

/// A phone-sized viewport.
///
/// 360x800 is a common Android size and is deliberately on the SMALL side: a
/// layout that survives here survives everywhere, and one tuned on a large
/// device overflows on the cheap phone most of the audience actually owns.
const goldenSurface = Size(360, 800);

/// A taller surface, for screens that legitimately do not fit at 200%.
///
/// Used only where the content is genuinely long and scrolls. A screen that
/// needs this at 100% has a layout problem, not a surface-size problem.
const goldenTallSurface = Size(360, 1600);

/// Wraps a widget in the app's real theme, localisations, and scale.
///
/// The theme is built here rather than imported from main.dart because main
/// also starts the app. Keeping them in step is what
/// `TestGoldenThemeMatchesProduction` in golden_test.dart asserts.
Widget goldenApp({
  required Widget child,
  required GoldenVariant variant,
  List<Override> overrides = const [],
}) {
  return ProviderScope(
    overrides: overrides,
    child: MaterialApp(
      debugShowCheckedModeBanner: false,
      locale: variant.locale,
      localizationsDelegates: const [
        L10n.delegate,
        GlobalMaterialLocalizations.delegate,
        GlobalWidgetsLocalizations.delegate,
        GlobalCupertinoLocalizations.delegate,
      ],
      supportedLocales: L10n.supportedLocales,
      theme: goldenTheme(variant.brightness),
      builder: (context, child) => MediaQuery(
        data: MediaQuery.of(context).copyWith(
          textScaler: TextScaler.linear(variant.textScale),
        ),
        child: child ?? const SizedBox.shrink(),
      ),
      home: child,
    ),
  );
}

/// The app's theme, mirroring main.dart.
ThemeData goldenTheme(Brightness brightness) {
  final scheme = ColorScheme.fromSeed(
    seedColor: const Color(0xFF6C4DF6),
    brightness: brightness,
  );

  return ThemeData(
    colorScheme: scheme,
    useMaterial3: true,
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

/// Sets the test surface and resets it afterwards.
Future<void> withSurface(
  WidgetTester tester,
  Size size,
  Future<void> Function() body,
) async {
  await tester.binding.setSurfaceSize(size);
  addTearDown(() => tester.binding.setSurfaceSize(null));
  await body();
}

/// Asserts nothing overflowed.
///
/// A RenderFlex overflow reports itself through the framework's exception
/// channel rather than by failing a widget lookup, so a test that only checks
/// for widgets would pass with a yellow-and-black stripe filling the screen.
void expectNoOverflow(WidgetTester tester, GoldenVariant variant) {
  final exception = tester.takeException();
  expect(
    exception,
    isNull,
    reason: 'the layout overflowed at ${variant.name}; '
        'NFR-16 requires 200% text scale to work',
  );
}
