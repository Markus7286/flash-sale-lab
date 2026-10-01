<?php

use App\Http\Controllers\ReserveController;
use Illuminate\Support\Facades\Route;

Route::get('/healthz', [ReserveController::class, 'healthz']);
Route::get('/backends', [ReserveController::class, 'backends']);

Route::post('/{version}/flash-sale', [ReserveController::class, 'reserve']);
Route::get('/{version}/skus/{sku}/stock', [ReserveController::class, 'stock']);
Route::put('/{version}/admin/skus/{sku}/stock', [ReserveController::class, 'seed']);

// Alias so curl and the smoke test need not know which backend is current.
Route::post('/flash-sale', [ReserveController::class, 'reserve']);
