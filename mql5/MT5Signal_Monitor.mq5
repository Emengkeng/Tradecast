//+------------------------------------------------------------------+
//| MT5Signal Monitor EA                                             |
//| Detects OPEN/MODIFY/CLOSE/PARTIAL events and POSTs to server.   |
//| Signs every request with HMAC-SHA256 to prove authenticity.     |
//| Persists seen tickets to a CSV file to survive EA restarts.     |
//+------------------------------------------------------------------+
#property copyright "MT4Signal"
#property version   "2.0"

input string ServerURL      = "http://your-vps-ip:8080/signal";
input string HMACSecret     = "your-hmac-secret-here"; // Must match SIGNAL_HMAC_SECRET on server
input int    PollIntervalMs = 300;
input string StateFile      = "mt5signal_state.csv";

struct TicketState {
   ulong  ticket;
   double lot;
   double sl;
   double tp;
};

TicketState knownTickets[];
int         ticketCount = 0;

//+------------------------------------------------------------------+
int OnInit() {
   LoadStateFromFile();
   EventSetMillisecondTimer(PollIntervalMs);
   Print("[MT5Signal] Monitor EA started. Loaded ", ticketCount, " known tickets.");
   return INIT_SUCCEEDED;
}

void OnDeinit(const int reason) {
   EventKillTimer();
   Print("[MT5Signal] Monitor EA stopped.");
}

void OnTimer() { ScanTrades(); }

//+------------------------------------------------------------------+
void ScanTrades() {
   // --- Detect closed / partial trades first ---
   for (int i = ticketCount - 1; i >= 0; i--) {
      bool stillOpen = false;

      for (int j = PositionsTotal() - 1; j >= 0; j--) {
         ulong ticket = PositionGetTicket(j);
         if (ticket == knownTickets[i].ticket) {
            stillOpen = true;

            double currentLot = PositionGetDouble(POSITION_VOLUME);
            double currentSL  = PositionGetDouble(POSITION_SL);
            double currentTP  = PositionGetDouble(POSITION_TP);
            string symbol     = PositionGetString(POSITION_SYMBOL);
            string direction  = (PositionGetInteger(POSITION_TYPE) == POSITION_TYPE_BUY) ? "BUY" : "SELL";
            double openPrice  = PositionGetDouble(POSITION_PRICE_OPEN);

            // Detect MODIFY
            if (MathAbs(currentSL - knownTickets[i].sl) > 0.000001 ||
                MathAbs(currentTP - knownTickets[i].tp) > 0.000001) {
               SendSignal(ticket, "MODIFY", symbol, direction,
                          openPrice, currentSL, currentTP, currentLot);
               knownTickets[i].sl = currentSL;
               knownTickets[i].tp = currentTP;
               SaveStateToFile();
            }

            // Detect PARTIAL close (lot decreased)
            if (currentLot < knownTickets[i].lot - 0.001) {
               SendSignal(ticket, "PARTIAL", symbol, direction,
                          openPrice, currentSL, currentTP, currentLot);
               knownTickets[i].lot = currentLot;
               SaveStateToFile();
            }
            break;
         }
      }

      if (!stillOpen) {
         // Look up close price from history
         if (HistoryDealSelect(knownTickets[i].ticket)) {
            // Try to find the closing deal via history
         }
         // Use HistorySelect to fetch recent history for close info
         datetime from = (datetime)(TimeCurrent() - 7 * 24 * 3600);
         HistorySelect(from, TimeCurrent());

         double closePrice = 0;
         string closeSymbol = "";
         string closeDirection = "";
         double closeLot = 0;
         double closeSL = 0;
         double closeTP = 0;

         // Find the matching position in deals history
         for (int d = HistoryDealsTotal() - 1; d >= 0; d--) {
            ulong dealTicket = HistoryDealGetTicket(d);
            if (HistoryDealGetInteger(dealTicket, DEAL_POSITION_ID) == (long)knownTickets[i].ticket) {
               if (HistoryDealGetInteger(dealTicket, DEAL_ENTRY) == DEAL_ENTRY_OUT ||
                   HistoryDealGetInteger(dealTicket, DEAL_ENTRY) == DEAL_ENTRY_INOUT) {
                  closePrice     = HistoryDealGetDouble(dealTicket, DEAL_PRICE);
                  closeSymbol    = HistoryDealGetString(dealTicket, DEAL_SYMBOL);
                  closeLot       = HistoryDealGetDouble(dealTicket, DEAL_VOLUME);
                  long dealType  = HistoryDealGetInteger(dealTicket, DEAL_TYPE);
                  closeDirection = (dealType == DEAL_TYPE_BUY) ? "BUY" : "SELL";
                  break;
               }
            }
         }

         if (closePrice > 0) {
            SendSignal(knownTickets[i].ticket, "CLOSE", closeSymbol, closeDirection,
                       closePrice, closeSL, closeTP, closeLot);
         }

         RemoveKnownTicket(i);
         SaveStateToFile();
      }
   }

   // --- Detect new OPEN positions ---
   for (int j = PositionsTotal() - 1; j >= 0; j--) {
      ulong ticket = PositionGetTicket(j);
      if (ticket == 0) continue;

      if (!IsKnownTicket(ticket)) {
         double lot       = PositionGetDouble(POSITION_VOLUME);
         double sl        = PositionGetDouble(POSITION_SL);
         double tp        = PositionGetDouble(POSITION_TP);
         string symbol    = PositionGetString(POSITION_SYMBOL);
         double openPrice = PositionGetDouble(POSITION_PRICE_OPEN);
         string direction = (PositionGetInteger(POSITION_TYPE) == POSITION_TYPE_BUY) ? "BUY" : "SELL";

         AddKnownTicket(ticket, lot, sl, tp);
         SendSignal(ticket, "OPEN", symbol, direction, openPrice, sl, tp, lot);
         SaveStateToFile();
      }
   }
}

//+------------------------------------------------------------------+
void SendSignal(ulong ticket, string sigType, string symbol, string direction,
                double price, double sl, double tp, double lot) {

   // Build RFC3339 timestamp
   MqlDateTime dt;
   TimeToStruct(TimeGMT(), dt);
   string timestamp = StringFormat("%04d-%02d-%02dT%02d:%02d:%02dZ",
                                   dt.year, dt.mon, dt.day,
                                   dt.hour, dt.min, dt.sec);

   string signature = ComputeHMAC(HMACSecret,
      IntegerToString((long)ticket) + ":" + sigType + ":" + symbol + ":" + timestamp);

   string payload = "{";
   payload += "\"ticket_id\":"    + IntegerToString((long)ticket) + ",";
   payload += "\"signal_type\":\"" + sigType    + "\",";
   payload += "\"symbol\":\""      + symbol     + "\",";
   payload += "\"direction\":\""   + direction  + "\",";
   payload += "\"price\":"         + DoubleToString(price, 8) + ",";
   if (sl > 0) payload += "\"sl\":" + DoubleToString(sl, 8) + ",";
   if (tp > 0) payload += "\"tp\":" + DoubleToString(tp, 8) + ",";
   payload += "\"lot\":"           + DoubleToString(lot, 4) + ",";
   payload += "\"timestamp\":\""   + timestamp + "\"";
   payload += "}";

   string headers = "Content-Type: application/json\r\n";
   headers += "X-Signal-Signature: " + signature + "\r\n";
   headers += "X-Signal-Timestamp: " + timestamp;

   char postData[];
   char result[];
   string resultHeaders;
   StringToCharArray(payload, postData, 0, StringLen(payload));

   int res = WebRequest("POST", ServerURL, headers, 5000, postData, result, resultHeaders);
   if (res == -1) {
      Print("[MT5Signal] WebRequest failed. Error: ", GetLastError(),
            ". Whitelist ", ServerURL, " in Tools > Options > Expert Advisors.");
      Print("[MT5Signal] Failed payload: ", payload);
   } else if (res != 200) {
      Print("[MT5Signal] Server returned ", res, ": ", CharArrayToString(result));
   } else {
      Print("[MT5Signal] Signal sent OK. Ticket:", ticket, " Type:", sigType, " Symbol:", symbol);
   }
}

//+------------------------------------------------------------------+
// HMAC-SHA256 via CryptEncode (available in MT5)
string ComputeHMAC(string key, string message) {
   uchar keyBytes[], msgBytes[], innerData[], outerData[];
   uchar innerHash[], finalHash[];
   uchar ipad[], opad[];
   int blockSize = 64; // SHA256 block size

   StringToCharArray(key,     keyBytes, 0, StringLen(key));
   StringToCharArray(message, msgBytes, 0, StringLen(message));

   // If key longer than block size, hash it first
   if (ArraySize(keyBytes) > blockSize) {
      uchar emptyKey[], hashedKey[];
      CryptEncode(CRYPT_HASH_SHA256, keyBytes, emptyKey, hashedKey);
      ArrayCopy(keyBytes, hashedKey);
   }

   // Pad key to block size
   ArrayResize(keyBytes, blockSize, 0);

   // Build ipad (0x36) and opad (0x5C) XOR'd with key
   ArrayResize(ipad, blockSize);
   ArrayResize(opad, blockSize);
   for (int i = 0; i < blockSize; i++) {
      ipad[i] = keyBytes[i] ^ 0x36;
      opad[i] = keyBytes[i] ^ 0x5C;
   }

   // Inner hash: SHA256(ipad + message)
   ArrayResize(innerData, blockSize + ArraySize(msgBytes));
   ArrayCopy(innerData, ipad, 0, 0, blockSize);
   ArrayCopy(innerData, msgBytes, blockSize, 0);
   uchar emptyKey[];
   CryptEncode(CRYPT_HASH_SHA256, innerData, emptyKey, innerHash);

   // Outer hash: SHA256(opad + innerHash)
   ArrayResize(outerData, blockSize + ArraySize(innerHash));
   ArrayCopy(outerData, opad, 0, 0, blockSize);
   ArrayCopy(outerData, innerHash, blockSize, 0);
   CryptEncode(CRYPT_HASH_SHA256, outerData, emptyKey, finalHash);

   // Convert to hex string
   string hexResult = "";
   for (int i = 0; i < ArraySize(finalHash); i++)
      hexResult += StringFormat("%02x", finalHash[i]);
   return hexResult;
}

//+------------------------------------------------------------------+
// Ticket state management
bool IsKnownTicket(ulong ticket) {
   for (int i = 0; i < ticketCount; i++)
      if (knownTickets[i].ticket == ticket) return true;
   return false;
}

void AddKnownTicket(ulong ticket, double lot, double sl, double tp) {
   ArrayResize(knownTickets, ticketCount + 1);
   knownTickets[ticketCount].ticket = ticket;
   knownTickets[ticketCount].lot    = lot;
   knownTickets[ticketCount].sl     = sl;
   knownTickets[ticketCount].tp     = tp;
   ticketCount++;
}

void RemoveKnownTicket(int index) {
   for (int i = index; i < ticketCount - 1; i++)
      knownTickets[i] = knownTickets[i + 1];
   ticketCount--;
   ArrayResize(knownTickets, ticketCount);
}

//+------------------------------------------------------------------+
// File persistence
void SaveStateToFile() {
   int handle = FileOpen(StateFile, FILE_WRITE | FILE_CSV | FILE_COMMON);
   if (handle == INVALID_HANDLE) {
      Print("[MT5Signal] Cannot open state file for writing");
      return;
   }
   for (int i = 0; i < ticketCount; i++) {
      FileWrite(handle,
         IntegerToString((long)knownTickets[i].ticket),
         DoubleToString(knownTickets[i].lot, 4),
         DoubleToString(knownTickets[i].sl, 8),
         DoubleToString(knownTickets[i].tp, 8)
      );
   }
   FileClose(handle);
}

void LoadStateFromFile() {
   if (!FileIsExist(StateFile, FILE_COMMON)) return;
   int handle = FileOpen(StateFile, FILE_READ | FILE_CSV | FILE_COMMON);
   if (handle == INVALID_HANDLE) return;

   ticketCount = 0;
   ArrayResize(knownTickets, 0);

   while (!FileIsEnding(handle)) {
      string ticketStr = FileReadString(handle);
      if (ticketStr == "") break;
      double lot = StringToDouble(FileReadString(handle));
      double sl  = StringToDouble(FileReadString(handle));
      double tp  = StringToDouble(FileReadString(handle));

      ulong ticket = (ulong)StringToInteger(ticketStr);

      // Only load if position is still open
      for (int j = PositionsTotal() - 1; j >= 0; j--) {
         if (PositionGetTicket(j) == ticket) {
            AddKnownTicket(ticket, lot, sl, tp);
            break;
         }
      }
   }
   FileClose(handle);
   Print("[MT5Signal] State loaded: ", ticketCount, " open positions.");
}