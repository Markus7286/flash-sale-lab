<?php

// Nothing on the reservation path caches; array keeps the default file store from
// touching disk if anything in the framework ever asks.
return [
    'default' => 'array',
];
