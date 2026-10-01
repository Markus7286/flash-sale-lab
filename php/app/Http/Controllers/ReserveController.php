<?php

namespace App\Http\Controllers;

use App\FlashSale\EloquentReserver;
use App\FlashSale\RawReserver;
use App\FlashSale\Reserver;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Log;
use JsonException;
use Throwable;

/**
 * internal/httpapi/router.go in Laravel: the same paths, headers, bodies, status
 * codes and error messages, so k6 drives either server by changing only BASE_URL.
 */
final class ReserveController
{
    // Served by the unversioned /flash-sale alias.
    private const FALLBACK = 'v0';

    // Matches maxBodyBytes in router.go.
    private const MAX_BODY_BYTES = 64 << 10;

    /** @var array<string, class-string<Reserver>> */
    private const BACKENDS = [
        'v0' => RawReserver::class,
        'v0b' => EloquentReserver::class,
    ];

    public function healthz(): JsonResponse
    {
        return $this->json(200, ['status' => 'ok']);
    }

    public function backends(): JsonResponse
    {
        $names = [];
        foreach (array_keys(self::BACKENDS) as $version) {
            $names[$version] = $this->reserver($version)?->name();
        }

        return $this->json(200, ['backends' => $names, 'default' => self::FALLBACK]);
    }

    public function reserve(Request $request, string $version = self::FALLBACK): JsonResponse
    {
        $reserver = $this->reserver($version);
        if ($reserver === null) {
            return $this->error(404, "unknown backend version {$version}");
        }

        $userId = (string) $request->headers->get('X-User-Id');
        $requestId = (string) $request->headers->get('X-Request-Id');
        if ($userId === '' || $requestId === '') {
            return $this->error(400, 'both X-User-Id and X-Request-Id headers are required');
        }

        $body = $this->decode($request);
        if ($body instanceof JsonResponse) {
            return $body;
        }
        $sku = $body['sku'] ?? '';
        $qty = $body['qty'] ?? 0;
        // Go rejects a mistyped field while decoding, with the decoder's message.
        if (! is_string($sku) || ! is_int($qty)) {
            return $this->error(400, 'malformed json body');
        }
        if ($sku === '') {
            return $this->error(400, 'sku is required');
        }
        if ($qty === 0) {
            $qty = 1;
        }
        if ($qty < 0) {
            return $this->error(400, 'qty must be positive');
        }

        try {
            $result = $reserver->reserve($sku, $userId, $requestId, $qty);
        } catch (Throwable $e) {
            Log::error('reserve failed', [
                'backend' => $reserver->name(), 'sku' => $sku, 'user_id' => $userId,
                'request_id' => $requestId, 'err' => $e->getMessage(),
            ]);

            return $this->error(500, 'reservation failed');
        }

        return $this->json($result->status->httpCode(), [
            'status' => $result->status->value,
            'backend' => $reserver->name(),
            'sku' => $sku,
            'request_id' => $requestId,
            'remaining' => $result->remaining,
        ])->header('X-Request-Id', $requestId);
    }

    public function stock(string $version, string $sku): JsonResponse
    {
        $reserver = $this->reserver($version);
        if ($reserver === null) {
            return $this->error(404, "unknown backend version {$version}");
        }

        try {
            $stats = $reserver->stats($sku);
        } catch (Throwable $e) {
            Log::error('read stats failed', ['backend' => $reserver->name(), 'sku' => $sku, 'err' => $e->getMessage()]);

            return $this->error(500, 'could not read stock');
        }
        if ($stats === null) {
            return $this->error(404, 'unknown sku');
        }

        return $this->json(200, [
            'backend' => $reserver->name(),
            'buyers' => $stats->buyers,
            'remaining' => $stats->remaining,
            'reservations' => $stats->reservations,
            'sku' => $sku,
            'units_sold' => $stats->unitsSold,
        ]);
    }

    public function seed(Request $request, string $version, string $sku): JsonResponse
    {
        $reserver = $this->reserver($version);
        if ($reserver === null) {
            return $this->error(404, "unknown backend version {$version}");
        }

        $body = $this->decode($request);
        if ($body instanceof JsonResponse) {
            return $body;
        }
        $stock = $body['stock'] ?? 0;
        if (! is_int($stock)) {
            return $this->error(400, 'malformed json body');
        }
        if ($stock < 0) {
            return $this->error(400, 'stock must not be negative');
        }

        try {
            $reserver->seedStock($sku, $stock);
        } catch (Throwable $e) {
            Log::error('seed stock failed', ['backend' => $reserver->name(), 'sku' => $sku, 'err' => $e->getMessage()]);

            return $this->error(500, 'could not seed stock');
        }
        Log::info('stock seeded', ['backend' => $reserver->name(), 'sku' => $sku, 'stock' => $stock]);

        return $this->json(200, ['backend' => $reserver->name(), 'sku' => $sku, 'stock' => $stock]);
    }

    private function reserver(string $version): ?Reserver
    {
        $class = self::BACKENDS[$version] ?? null;

        return $class === null ? null : new $class((int) config('flashsale.per_user_limit'));
    }

    /**
     * Decodes the raw body rather than $request->json(), which turns malformed JSON
     * into an empty bag where Go answers 400.
     *
     * @return array<string, mixed>|JsonResponse
     */
    private function decode(Request $request): array|JsonResponse
    {
        $content = $request->getContent();
        if (strlen($content) > self::MAX_BODY_BYTES) {
            return $this->error(413, 'request body too large');
        }

        try {
            $body = json_decode($content, true, 512, JSON_THROW_ON_ERROR);
        } catch (JsonException) {
            return $this->error(400, 'malformed json body');
        }
        // A JSON null decodes into Go's zero-value struct; anything but an object fails.
        if ($body === null) {
            return [];
        }
        if (! is_array($body) || ($body !== [] && array_is_list($body))) {
            return $this->error(400, 'malformed json body');
        }

        /** @var array<string, mixed> $body */
        return $body;
    }

    /**
     * Callers list keys alphabetically wherever router.go encodes a map, which Go
     * sorts, so both servers emit byte-identical bodies.
     *
     * @param  array<string, mixed>  $payload
     */
    private function json(int $code, array $payload): JsonResponse
    {
        return new JsonResponse($payload, $code);
    }

    private function error(int $code, string $message): JsonResponse
    {
        return $this->json($code, ['error' => $message]);
    }
}
