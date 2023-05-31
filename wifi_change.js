/**
 * @description
 * 如果是家里WI-FI则开启直连模式
 * 如果不是家里WI-FI则开启代理模式
 */
const WIFI_DONT_NEED_PROXYS = ['wifi_123456_5G'];

if (WIFI_DONT_NEED_PROXYS.includes($network.wifi.ssid)) {
  $surge.setOutboundMode('direct');
  $notification.post('Surge', 'Wi-Fi changed', 'use direct mode');
} else {
  $surge.setSelectGroupPolicy('Final-select', 'Group');
  $surge.setOutboundMode('rule');
  $notification.post('Surge', 'Wi-Fi changed', 'use rule-based proxy mode');
}

$done();
