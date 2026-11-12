package meshcore

import "fmt"

// ============================================================================
// Вспомогательные функции
// ============================================================================

func pubKeyHex(key []byte) string {
	return fmt.Sprintf("%x", key)
}

// ============================================================================
// Установка коллбэков
// ============================================================================

func (c *MeshCoreClient) OnContact(f func(*Contact)) {
	c.onContact = f
}

// OnContactDeleted вызывается, когда устройство удалило контакт само: push
// 0x8F (PUSH_CODE_CONTACT_DELETED) приходит при вытеснении контакта при
// переполнении хранилища. pubKey — полные 32 байта удалённого контакта.
// Явный RemoveContact этот колбэк не вызывает — его результат и так виден
// вызывающему.
func (c *MeshCoreClient) OnContactDeleted(f func(pubKey []byte)) {
	c.onContactDeleted = f
}
func (c *MeshCoreClient) OnAdvert(f func(*Contact)) {
	c.onAdvert = f
}
func (c *MeshCoreClient) OnChannel(f func(*Channel)) {
	c.onChannel = f
}
func (c *MeshCoreClient) OnMessage(f func(*Message)) {
	c.onMessage = f
}
func (c *MeshCoreClient) OnChannelMessage(f func(*ChannelMessage)) {
	c.onChannelMessage = f
}
func (c *MeshCoreClient) OnSelfInfo(f func(*SelfInfo)) {
	c.onSelfInfo = f
}
func (c *MeshCoreClient) OnStats(f func(*CompanionRadioStats)) {
	c.onStats = f
}
func (c *MeshCoreClient) OnBattery(f func(millivolts int, usedKb, totalKb int)) {
	c.onBattery = f
}
func (c *MeshCoreClient) OnError(f func(error)) {
	c.onError = f
}
